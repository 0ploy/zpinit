package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

type replicasWrapper struct {
	Replicas Replicas `toml:"replicas"`
}

func decodeReplicas(t *testing.T, s string) (Replicas, error) {
	t.Helper()
	var w replicasWrapper
	_, err := toml.Decode(s, &w)
	return w.Replicas, err
}

func TestReplicasUnmarshal_Integer(t *testing.T) {
	r, err := decodeReplicas(t, `replicas = 3`)
	if err != nil {
		t.Fatal(err)
	}
	if r.N != 3 || r.Auto {
		t.Errorf("got %+v, want {N:3 Auto:false}", r)
	}
}

func TestReplicasUnmarshal_Auto(t *testing.T) {
	r, err := decodeReplicas(t, `replicas = "auto"`)
	if err != nil {
		t.Fatal(err)
	}
	if r.N != 0 || !r.Auto {
		t.Errorf("got %+v, want {N:0 Auto:true}", r)
	}
}

func TestReplicasUnmarshal_UnknownString(t *testing.T) {
	_, err := decodeReplicas(t, `replicas = "manual"`)
	if err == nil {
		t.Fatal("expected error for unknown string")
	}
	if !strings.Contains(err.Error(), "manual") {
		t.Errorf("error should mention the bad value: %v", err)
	}
}

func TestReplicasUnmarshal_NegativeInt(t *testing.T) {
	_, err := decodeReplicas(t, `replicas = -2`)
	if err == nil {
		t.Fatal("expected error for negative int")
	}
}

func TestReplicasUnmarshal_BoolRejected(t *testing.T) {
	_, err := decodeReplicas(t, `replicas = true`)
	if err == nil {
		t.Fatal("expected error for bool")
	}
}
