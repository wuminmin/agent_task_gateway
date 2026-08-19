package main

import (
	"context"
	"testing"
)

func TestScaleDeploymentFreePathsDoNotEagerlyAcquireProfileServices(t *testing.T) {
	for _, name := range []string{"TASKBOUND_ALICE_TOKEN", "TASKBOUND_CAROL_TOKEN", "OA_ALICE_PASSWORD",
		"OA_BOB_PASSWORD", "TASKGATE_FINAL_V5_BUSINESS_DSN", "TASKGATE_FINAL_V5_CONTROL_DSN",
		"TASKGATE_FINAL_V5_BUSINESS_OBSERVER_DSN", "TASKGATE_FINAL_V5_OBJECT_STORE_URL",
		"TASKGATE_FINAL_V5_OBJECT_STORE_ACCESS_KEY", "TASKGATE_FINAL_V5_OBJECT_STORE_SECRET_KEY",
		"TASKGATE_FINAL_V5_OBJECT_STORE_BUCKET"} {
		t.Setenv(name, "")
	}
	adapter, err := newScaleAdapter(context.Background())
	if err != nil {
		t.Fatalf("construct deployment-free Scale adapter: %v", err)
	}
	scale := adapter.(*scaleAdapter)
	if scale.real != nil {
		t.Fatal("Scale adapter eagerly acquired the profile/Gateway service bundle")
	}
	scale.Close()
}
