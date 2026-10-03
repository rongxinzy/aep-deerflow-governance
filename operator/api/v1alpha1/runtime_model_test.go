package v1alpha1

import "testing"

func TestRuntimeModelDeepCopy(t *testing.T) {
	full := RuntimeModel{
		Name: "bench-anthropic", Provider: "anthropic", NativeModel: "glm-5.3-flash",
		BaseURL: "http://gateway/bench-anthropic", ApiKeyEnv: "BIGMODEL_API_KEY",
	}
	clone := full.DeepCopy()
	if *clone != full {
		t.Fatalf("DeepCopy() = %#v", clone)
	}
	clone.NativeModel = "mutated"
	if full.NativeModel == "mutated" {
		t.Fatal("DeepCopy() returned an alias")
	}

	var empty RuntimeModel
	if got := empty.DeepCopy(); *got != empty {
		t.Fatalf("empty DeepCopy() = %#v", got)
	}

	spec := RuntimeSpec{Models: []RuntimeModel{full}}
	specCopy := *spec.DeepCopy()
	if len(specCopy.Models) != 1 || specCopy.Models[0] != full {
		t.Fatalf("RuntimeSpec.DeepCopy() models = %#v", specCopy.Models)
	}
	specCopy.Models[0].Name = "mutated"
	if spec.Models[0].Name == "mutated" {
		t.Fatal("RuntimeSpec.DeepCopy() aliased the models slice")
	}
}
