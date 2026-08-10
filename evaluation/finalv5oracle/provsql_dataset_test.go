package finalv5oracle

import (
	"slices"
	"testing"
)

func TestProvSQLDatasetIsTheFrozenThreeProductPrefix(t *testing.T) {
	specs := ProvSQLDatasetProductSpecs()
	if len(specs) != 3 {
		t.Fatalf("ProvSQL Product count = %d, want 3", len(specs))
	}
	wantIDs := []string{ProvSQLOrdersProductID, ProvSQLLineitemProductID, ProvSQLNonceProductID}
	wantRows := []int64{50_000, 250_000, 1_000}
	wantFields := [][]string{
		{"orderkey", "status", "partition_key"},
		{"orderkey", "linenumber", "extendedprice", "partition_key"},
		{"nonce_id", "partition_key"},
	}
	for index, spec := range specs {
		if spec.ProductID != wantIDs[index] || spec.Snapshot != ProvSQLSnapshot || spec.RowCount != wantRows[index] {
			t.Fatalf("Product %d = %+v", index, spec)
		}
		gotFields := make([]string, len(spec.Fields))
		for field := range spec.Fields {
			gotFields[field] = spec.Fields[field].Name
		}
		if !slices.Equal(gotFields, wantFields[index]) {
			t.Fatalf("Product %s fields = %v, want %v", spec.ProductID, gotFields, wantFields[index])
		}
	}

	summary, err := ProvSQLDatasetFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProductCount != 3 || summary.RowCount != ProvSQLDatasetRows || summary.PeakBufferedRows != 1 || !validSHA256(summary.SHA256) {
		t.Fatalf("ProvSQL Dataset summary = %+v", summary)
	}
	for _, product := range summary.Products {
		if !validSHA256(product.SHA256) {
			t.Fatalf("Product fingerprint = %+v", product)
		}
	}
}

func TestProvSQLLiveTypedStreamsAgreeWithIndependentProducts(t *testing.T) {
	products, err := provSQLDatasetProducts()
	if err != nil {
		t.Fatal(err)
	}
	streams := make(map[string]DatasetRowStream, len(products))
	for _, product := range products {
		product := product
		streams[product.productID] = func(yield func([]any) error) error {
			for rowIndex := int64(0); rowIndex < product.rowCount; rowIndex++ {
				row, rowErr := product.row(rowIndex)
				if rowErr != nil {
					return rowErr
				}
				if err := yield(row); err != nil {
					return err
				}
			}
			return nil
		}
	}
	want, err := ProvSQLDatasetFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ProvSQLDatasetFingerprintFromStreams(streams)
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA256 != want.SHA256 || !slices.Equal(got.Products, want.Products) {
		t.Fatalf("typed live agreement = %+v, want %+v", got, want)
	}

	delete(streams, ProvSQLNonceProductID)
	if _, err := ProvSQLDatasetFingerprintFromStreams(streams); err == nil {
		t.Fatal("incomplete ProvSQL Product stream set was accepted")
	}
}
