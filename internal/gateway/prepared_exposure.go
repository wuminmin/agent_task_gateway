package gateway

import (
	"errors"
	"fmt"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

// This file is the whole of what the Gateway does with a preparation.
//
// Before T1d the Gateway derived the statements, the projections, the metering
// closure, the normal form, the ordinal program, the sidecar SQL, the widened
// policy grant and the V5 predicate footprint itself, and internal/physicalquery
// derived all of it a second time so a differential harness could compare them.
// Two implementations that agree in tests can still diverge in the one execution
// nobody reproduced, which is why the extraction was never going to end with
// both of them running.
//
// What is left here is the mapping. Nothing below computes a statement, a
// projection, a digest or an authorization; each is read off the sealed
// PreparedOperation. The only values this file introduces are the ones
// preparation cannot hold: the Catalog Products the observation reads types and
// entity keys from, and the live SnapshotIndex handles the ordinal deriver
// resolves row handles through.

// planExposureContextFrom builds the Gateway's observation state from a sealed
// preparation.
//
// plan is the plan the observation is expressed in, which is not always the plan
// that was prepared: a semantic View prepares from the agent's outer plan but
// observes against the composed one, because the composed plan is what the
// statements project and group by.
func (s *Service) planExposureContextFrom(prepared physicalquery.PreparedOperation,
	plan queryplan.QueryPlan) (*planExposureContext, error) {
	// ExecutableStatements validates first, so a preparation whose runtime half
	// and binding have come apart cannot become an execution context.
	visibleSQL, companionSQL, err := prepared.ExecutableStatements()
	if err != nil {
		return nil, err
	}
	binding := prepared.Binding()
	context := &planExposureContext{
		plan:              cloneQueryPlan(plan),
		mainSQL:           visibleSQL,
		provenanceSQL:     companionSQL,
		visibleFields:     prepared.VisibleFields(),
		factFields:        prepared.FactFields(),
		provenanceFields:  prepared.ProvenanceFields(),
		grouped:           prepared.Grouped(),
		expandedEvidence:  prepared.ExpandedEvidence(),
		planDigest:        binding.NormalFormSHA256,
		viewBindingDigest: binding.ViewBindingSHA256,
		policyGrant:       prepared.PolicyGrant(),
		prepared:          prepared,
	}
	// viewRegistryRevision is deliberately not filled here. The binding carries a
	// DIGEST of the revision -- an operational string is not evidence -- while the
	// Gateway re-checks the revision itself against the registry it is about to
	// execute against, so it is set by the View path from the resolved binding.
	if context.predicateFootprint, err = prepared.PredicateFootprint(); err != nil {
		return nil, err
	}
	compilation, err := prepared.RelationalCompilation()
	if err != nil {
		return nil, err
	}
	if compilation == nil {
		product, present := s.catalog.LookupProduct(plan.Product)
		if !present {
			return nil, fmt.Errorf("this Catalog no longer declares prepared product %q", plan.Product)
		}
		context.product = product
	} else {
		products := make(map[string]catalog.Product, len(compilation.Products))
		for _, name := range compilation.Products {
			product, present := s.catalog.LookupProduct(name)
			if !present {
				return nil, fmt.Errorf("this Catalog no longer declares prepared product %q", name)
			}
			products[name] = product
		}
		context.relational = &relationalExposureContext{compilation: *compilation, products: products}
	}
	if context.ordinal, err = s.boundOrdinalExecutionFrom(prepared, companionSQL); err != nil {
		return nil, err
	}
	return context, nil
}

// requirePreparedGrantStillCurrent refuses to execute a preparation the task
// authorization has moved out from under.
//
// Preparation seals a digest of the grant it read. This maps the grant loaded
// for execution through the same function and requires the two to agree, so a
// task whose approval narrowed between the two reads is refused rather than
// executed against the wider surface preparation compiled for. It is a real
// window: the two loads are separate statements against the Control Store, and
// nothing between them holds the task.
func requirePreparedGrantStillCurrent(context *planExposureContext, grant control.TaskGrant) error {
	mapped := preparationGrant(grant)
	if context.viewBindingDigest != "" {
		mapped = viewPreparationGrant(grant, context.viewBindingDigest, context.viewRegistryRevision)
	}
	current, err := mapped.SHA256()
	if err != nil {
		return err
	}
	sealed := context.prepared.Binding().GrantSHA256
	if current != sealed {
		return fmt.Errorf("this operation was prepared under task grant %s but the grant now reads %s",
			sealed[:12], current[:12])
	}
	return nil
}

// boundOrdinalExecutionFrom reattaches the live snapshot handles to a prepared
// ordinal program, and returns nil for a profile that compiled none.
func (s *Service) boundOrdinalExecutionFrom(prepared physicalquery.PreparedOperation,
	companionSQL string) (*boundOrdinalExecution, error) {
	program, err := prepared.OrdinalProgram()
	if err != nil {
		return nil, err
	}
	if program == nil {
		return nil, nil
	}
	set, err := prepared.DictionarySet()
	if err != nil {
		return nil, err
	}
	if set == nil {
		return nil, errors.New("a prepared ordinal program resolved no dictionary universe")
	}
	digest, err := set.Digest()
	if err != nil {
		return nil, err
	}
	indexes, err := s.resolveOrdinalIndexes(prepared.SourcePublications())
	if err != nil {
		return nil, err
	}
	return &boundOrdinalExecution{
		Program: *program, ProvenanceSQL: companionSQL,
		ProvenanceFields: prepared.ProvenanceFields(),
		Indexes:          indexes,
		DictionarySet:    *set, DictionarySetDigest: digest,
		SidecarGrants:      prepared.SidecarGrants(),
		EstimatedBaseFacts: prepared.EstimatedBaseFacts(),
	}, nil
}
