package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/gateway"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/resultartifact"
)

// countedProductionConnector observes calls at the production Service's
// connector boundary. It never fabricates a response: every counted call is
// delegated to the real PostgreSQL connector. A replay that does not cross
// this boundary therefore has a directly observed BusinessSQLDelta of zero.
type productionConnector interface {
	Query(context.Context, dataconnector.QueryRequest) (dataconnector.Result, error)
	QueryPair(context.Context, dataconnector.QueryPairRequest) (dataconnector.QueryPairResult, error)
	QueryPairStream(context.Context, dataconnector.QueryPairStreamRequest) (dataconnector.QueryPairStreamResult, error)
	Ping(context.Context) error
	Attestation(context.Context) (dataconnector.Attestation, error)
	Close()
}

type countedProductionConnector struct {
	inner productionConnector
	calls atomic.Int64
}

var (
	_ productionConnector = (*dataconnector.Connector)(nil)
	_ productionConnector = (*countedProductionConnector)(nil)
)

func (connector *countedProductionConnector) Query(ctx context.Context,
	request dataconnector.QueryRequest) (dataconnector.Result, error) {
	connector.calls.Add(1)
	return connector.inner.Query(ctx, request)
}

func (connector *countedProductionConnector) QueryPair(ctx context.Context,
	request dataconnector.QueryPairRequest) (dataconnector.QueryPairResult, error) {
	connector.calls.Add(2)
	return connector.inner.QueryPair(ctx, request)
}

func (connector *countedProductionConnector) QueryPairStream(ctx context.Context,
	request dataconnector.QueryPairStreamRequest) (dataconnector.QueryPairStreamResult, error) {
	connector.calls.Add(2)
	return connector.inner.QueryPairStream(ctx, request)
}

func (connector *countedProductionConnector) Ping(ctx context.Context) error {
	return connector.inner.Ping(ctx)
}

func (connector *countedProductionConnector) Attestation(ctx context.Context) (dataconnector.Attestation, error) {
	return connector.inner.Attestation(ctx)
}

func (connector *countedProductionConnector) Close() { connector.inner.Close() }

func (connector *countedProductionConnector) Count() int64 { return connector.calls.Load() }
func (connector *countedProductionConnector) Reset()       { connector.calls.Store(0) }

type finalV5RuntimeFactory struct {
	store                 *control.Store
	approval              approval.ApprovalAdapter
	principal             mcp.Principal
	inputDir              string
	artifactRoots         map[string]string
	businessDSNs          map[string]string
	manager               *resultartifact.Manager
	signer                *queryreceipt.Signer
	logger                *slog.Logger
	callbackSecret        string
	receiptVerifier       approval.ReceiptVerifier
	callbackListenAddress string
	deliverySigningKey    []byte
}

type finalV5Runtime struct {
	mu          sync.Mutex
	service     *gateway.Service
	connector   *countedProductionConnector
	publication loadedPublication
	catalog     *catalog.Catalog
	cancel      context.CancelFunc
	callback    *http.Server
	callbackErr chan error
	closed      bool
}

func (factory *finalV5RuntimeFactory) Start(ctx context.Context,
	target sequentialSlotTarget) (sequentialSlotRuntime, error) {
	if factory == nil || factory.store == nil || factory.approval == nil || factory.manager == nil ||
		factory.signer == nil || factory.principal.ID == "" || len(factory.callbackSecret) < 32 ||
		factory.receiptVerifier == nil || factory.callbackListenAddress == "" || len(factory.deliverySigningKey) < 32 {
		return nil, errors.New("final V5 RQ5 runtime factory is incomplete")
	}
	artifactRoot := factory.artifactRoots[target.Day]
	dsn := factory.businessDSNs[target.Day]
	if artifactRoot == "" || dsn == "" {
		return nil, errors.New("final V5 RQ5 runtime target has no artifact root or Business DSN")
	}
	publication, err := loadVerifiedPublication(target.Day, factory.inputDir, artifactRoot)
	if err != nil {
		return nil, fmt.Errorf("load active %s publication: %w", target.Day, err)
	}
	logicalCatalog, _, err := catalogArtifact(publication)
	if err != nil {
		return nil, err
	}
	if logicalCatalog.SHA256 != target.CatalogSHA256 || publication.Bundle.ManifestDigest != target.PublicationSHA256 ||
		publication.Bundle.Hot.Bytes != target.HOTArtifactBytes {
		return nil, errors.New("active RQ5 publication differs from the single-slot target")
	}
	connector, attestation, err := openPublicationConnector(ctx, dsn, publication.Input,
		publication.Input.Snapshot.SchemaDigest, businessDatabase+"_"+target.Day)
	if err != nil {
		return nil, err
	}
	closeOnError := func(value error) (sequentialSlotRuntime, error) {
		connector.Close()
		return nil, value
	}
	if attestation.SchemaDigest != publication.Bundle.DictionaryManifest.SchemaDigest {
		return closeOnError(errors.New("active connector schema differs from measured publication"))
	}
	if err := verifyInstalledSidecar(ctx, connector, publication); err != nil {
		return closeOnError(fmt.Errorf("active installed sidecar: %w", err))
	}
	registry, err := ordinal.NewRegistry(publication.Index)
	if err != nil {
		return closeOnError(err)
	}
	if err := registry.RegisterPublication(ordinal.PublicationKey{
		CatalogDigest: logicalCatalog.SHA256, PublicationName: publication.Input.PublicationName,
	}, publication.Bundle.ManifestDigest, publication.Index); err != nil {
		return closeOnError(err)
	}
	if err := factory.store.EnforceExposureDeploymentMode(ctx, logicalCatalog.SHA256, true); err != nil {
		return closeOnError(err)
	}
	if err := factory.store.PutOrdinalSnapshotPublication(ctx, publication.Bundle.ManifestDigest,
		publication.Index, nil); err != nil {
		return closeOnError(err)
	}
	observed := &countedProductionConnector{inner: connector}
	background, cancel := context.WithCancel(context.Background())
	service, err := gateway.New(gateway.Config{
		Catalog: logicalCatalog, Store: factory.store, Approval: factory.approval,
		Connector: observed, SnapshotRegistry: registry, CallbackSecret: factory.callbackSecret,
		Logger: factory.logger, Clock: time.Now, Background: background,
		ReceiptVerifier: factory.receiptVerifier, QueryReceiptSigner: factory.signer, ResultArtifacts: factory.manager,
		ArtifactOperationTimeout: 2 * time.Minute, ResultTTL: time.Hour,
		DeliveryBaseURL: "http://127.0.0.1.invalid", DeliverySigningKey: factory.deliverySigningKey,
	})
	if err != nil {
		cancel()
		return closeOnError(err)
	}
	if _, err := service.ReconcilePendingArtifacts(ctx); err != nil {
		cancel()
		return closeOnError(fmt.Errorf("reconcile pending RQ5 artifacts: %w", err))
	}
	if err := service.ReadyError(); err != nil {
		cancel()
		return closeOnError(fmt.Errorf("RQ5 production service is not ready: %w", err))
	}
	listener, err := net.Listen("tcp", factory.callbackListenAddress)
	if err != nil {
		cancel()
		return closeOnError(fmt.Errorf("listen for real OA callback: %w", err))
	}
	mux := http.NewServeMux()
	mux.Handle("/api/v1/oa/callback", service.OACallbackHandler())
	callbackServer := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	callbackErrors := make(chan error, 1)
	go func() {
		callbackErrors <- callbackServer.Serve(listener)
	}()
	// Setup probes are outside the public-query observation window.
	observed.Reset()
	return &finalV5Runtime{service: service, connector: observed, publication: publication,
		catalog: logicalCatalog, cancel: cancel, callback: callbackServer, callbackErr: callbackErrors}, nil
}

func (runtimeValue *finalV5Runtime) Close() error {
	if runtimeValue == nil {
		return nil
	}
	runtimeValue.mu.Lock()
	defer runtimeValue.mu.Unlock()
	if runtimeValue.closed {
		return nil
	}
	runtimeValue.closed = true
	var closeErr error
	if runtimeValue.callback != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := runtimeValue.callback.Shutdown(shutdownCtx); err != nil {
			closeErr = err
		}
		cancel()
		if runtimeValue.callbackErr != nil {
			select {
			case err := <-runtimeValue.callbackErr:
				if err != nil && !errors.Is(err, http.ErrServerClosed) && closeErr == nil {
					closeErr = err
				}
			case <-time.After(5 * time.Second):
				if closeErr == nil {
					closeErr = errors.New("real OA callback server did not stop synchronously")
				}
			}
		}
	}
	if runtimeValue.cancel != nil {
		runtimeValue.cancel()
	}
	if runtimeValue.connector != nil && runtimeValue.connector.inner != nil {
		runtimeValue.connector.Close()
	}
	// Drop every reference that owns the active HOT dictionary before the slot
	// permits another Start. This is the synchronous stop boundary.
	runtimeValue.service = nil
	runtimeValue.connector = nil
	runtimeValue.catalog = nil
	runtimeValue.publication = loadedPublication{}
	runtimeValue.callback = nil
	runtimeValue.callbackErr = nil
	runtime.GC()
	return closeErr
}

func activeFinalV5Runtime(slot *singleGatewayServiceSlot) (*finalV5Runtime, error) {
	active, _, err := slot.Active()
	if err != nil {
		return nil, err
	}
	value, ok := active.(*finalV5Runtime)
	if !ok || value.service == nil || value.connector == nil || value.catalog == nil {
		return nil, errors.New("single RQ5 slot does not contain a live production runtime")
	}
	return value, nil
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
