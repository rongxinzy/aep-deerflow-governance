package main

import (
	"fmt"
	"regexp"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Configuration changes reboot the guest; the data disk is retained. Disk
// resizing is deliberately not exposed until the node supports offline resize.
func applyWorkspaceConfiguration(spec *dev1alpha1.PersonalWorkspaceSpec, input workspaceConfiguration, existing bool) error {
	if spec.Runtime == "" {
		spec.Runtime = "pod"
	}
	if spec.Runtime != "pod" && spec.Runtime != "microvm" {
		return fmt.Errorf("runtime must be pod or microvm")
	}
	if input.Desktop != nil {
		spec.Desktop = *input.Desktop
	}
	if input.TemplateTag != nil {
		spec.TemplateTag = *input.TemplateTag
	}
	if spec.TemplateTag != "" && !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`).MatchString(spec.TemplateTag) {
		return fmt.Errorf("invalid template tag")
	}
	if spec.Runtime != "microvm" && (spec.Desktop.Enabled || spec.TemplateTag != "") {
		return fmt.Errorf("desktop and template selection require a microvm workspace")
	}
	if input.Resources != nil {
		next := *input.Resources
		if next.Memory == "" {
			next.Memory = spec.Resources.Memory
		}
		if next.Disk == "" {
			next.Disk = spec.Resources.Disk
		}
		if next.VCPUs == 0 {
			next.VCPUs = spec.Resources.VCPUs
		}
		if existing && next.Disk != spec.Resources.Disk {
			return fmt.Errorf("existing data disk cannot be resized through this API")
		}
		spec.Resources = next
	}
	if !existing && spec.Runtime == "microvm" {
		if spec.Resources.Memory == "" {
			spec.Resources.Memory = "4Gi"
		}
		if spec.Resources.Disk == "" {
			spec.Resources.Disk = "20Gi"
		}
		if spec.Resources.VCPUs == 0 {
			spec.Resources.VCPUs = 2
		}
	}
	if spec.Resources.VCPUs < 0 || spec.Resources.VCPUs > 8 {
		return fmt.Errorf("vcpus must be between 1 and 8")
	}
	for _, item := range []struct{ name, value, min, max string }{
		{"memory", spec.Resources.Memory, "512Mi", "32Gi"},
		{"disk", spec.Resources.Disk, "2Gi", "100Gi"},
	} {
		if item.value == "" {
			continue
		}
		quantity, err := resource.ParseQuantity(item.value)
		if err != nil || quantity.Cmp(resource.MustParse(item.min)) < 0 || quantity.Cmp(resource.MustParse(item.max)) > 0 {
			return fmt.Errorf("%s must be between %s and %s", item.name, item.min, item.max)
		}
	}
	if spec.Desktop.Enabled {
		memory, err := resource.ParseQuantity(spec.Resources.Memory)
		if err != nil || memory.Cmp(resource.MustParse("4Gi")) < 0 || spec.Resources.VCPUs < 2 {
			return fmt.Errorf("desktop requires at least 2 vcpus and 4Gi memory")
		}
	}
	return nil
}
