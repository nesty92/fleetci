package gitlab

import (
	"encoding/json"
	"testing"

	"gitlab.com/gitlab-org/gitlab-runner/common/spec"
)

func TestMarshalJobRoundTrip(t *testing.T) {
	in := `{"id": 42, "token": "glcbt-x", "inputs": [
		{"key": "name", "value": {"type": "string", "content": "fleet", "sensitive": false}},
		{"key": "n", "value": {"type": "number", "content": 3, "sensitive": false}},
		{"key": "secret", "value": {"type": "string", "content": "s", "sensitive": true}}],
		"variables": [{"key": "FLEETCI_CLASS", "value": "4vcpu-8gb"}]}`
	var job spec.Job
	if err := json.Unmarshal([]byte(in), &job); err != nil {
		t.Fatal(err)
	}
	b, err := marshalJob(&job)
	if err != nil {
		t.Fatal(err)
	}
	var back spec.Job
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if back.ID != 42 || back.Token != "glcbt-x" || back.Inputs.Len() != 3 || back.Variables.Value("FLEETCI_CLASS") != "4vcpu-8gb" {
		t.Fatalf("round trip lost something: %s", b)
	}
	v, _ := back.Inputs.Attr("secret")
	if v.String() != "s" {
		t.Fatalf("secret %v", v)
	}
	var none spec.Job
	if b, err = marshalJob(&none); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &none); err != nil {
		t.Fatalf("empty job: %v\n%s", err, b)
	}
}
