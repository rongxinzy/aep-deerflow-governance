package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
)

// Hand-written deepcopy (the type set is small and stable; avoids a
// controller-tools codegen dependency in this module).

func (in *RuntimeSpec) DeepCopyInto(out *RuntimeSpec) {
	*out = *in
}

func (in *RuntimeSpec) DeepCopy() *RuntimeSpec {
	if in == nil {
		return nil
	}
	out := new(RuntimeSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *MemorySpec) DeepCopyInto(out *MemorySpec) {
	*out = *in
}

func (in *MemorySpec) DeepCopy() *MemorySpec {
	if in == nil {
		return nil
	}
	out := new(MemorySpec)
	in.DeepCopyInto(out)
	return out
}

func (in *KnowledgeSpec) DeepCopyInto(out *KnowledgeSpec) {
	*out = *in
}

func (in *KnowledgeSpec) DeepCopy() *KnowledgeSpec {
	if in == nil {
		return nil
	}
	out := new(KnowledgeSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *GovernanceSpec) DeepCopyInto(out *GovernanceSpec) {
	*out = *in
	if in.GovernedTools != nil {
		out.GovernedTools = append([]string(nil), in.GovernedTools...)
	}
	if in.TeamNames != nil {
		out.TeamNames = make(map[string]string, len(in.TeamNames))
		for k, v := range in.TeamNames {
			out.TeamNames[k] = v
		}
	}
}

func (in *GovernanceSpec) DeepCopy() *GovernanceSpec {
	if in == nil {
		return nil
	}
	out := new(GovernanceSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *CredentialsRef) DeepCopyInto(out *CredentialsRef) {
	*out = *in
}

func (in *CredentialsRef) DeepCopy() *CredentialsRef {
	if in == nil {
		return nil
	}
	out := new(CredentialsRef)
	in.DeepCopyInto(out)
	return out
}

func (in *DigitalEmployeeSpec) DeepCopyInto(out *DigitalEmployeeSpec) {
	*out = *in
	out.Runtime = *in.Runtime.DeepCopy()
	if in.Memory != nil {
		out.Memory = new(MemorySpec)
		in.Memory.DeepCopyInto(out.Memory)
	}
	if in.Knowledge != nil {
		out.Knowledge = new(KnowledgeSpec)
		in.Knowledge.DeepCopyInto(out.Knowledge)
	}
	in.Governance.DeepCopyInto(&out.Governance)
	out.Credentials = *in.Credentials.DeepCopy()
	if in.A2A != nil {
		out.A2A = new(A2ASpec)
		in.A2A.DeepCopyInto(out.A2A)
	}
}

func (in *DigitalEmployeeSpec) DeepCopy() *DigitalEmployeeSpec {
	if in == nil {
		return nil
	}
	out := new(DigitalEmployeeSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *DigitalEmployeeStatus) DeepCopyInto(out *DigitalEmployeeStatus) {
	*out = *in
}

func (in *DigitalEmployeeStatus) DeepCopy() *DigitalEmployeeStatus {
	if in == nil {
		return nil
	}
	out := new(DigitalEmployeeStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *DigitalEmployee) DeepCopyInto(out *DigitalEmployee) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	out.Status = *in.Status.DeepCopy()
}

func (in *DigitalEmployee) DeepCopy() *DigitalEmployee {
	if in == nil {
		return nil
	}
	out := new(DigitalEmployee)
	in.DeepCopyInto(out)
	return out
}

func (in *DigitalEmployee) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *DigitalEmployeeList) DeepCopyInto(out *DigitalEmployeeList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]DigitalEmployee, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *DigitalEmployeeList) DeepCopy() *DigitalEmployeeList {
	if in == nil {
		return nil
	}
	out := new(DigitalEmployeeList)
	in.DeepCopyInto(out)
	return out
}

func (in *DigitalEmployeeList) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *A2ASpec) DeepCopyInto(out *A2ASpec) {
	*out = *in
}

func (in *A2ASpec) DeepCopy() *A2ASpec {
	if in == nil {
		return nil
	}
	out := new(A2ASpec)
	in.DeepCopyInto(out)
	return out
}
