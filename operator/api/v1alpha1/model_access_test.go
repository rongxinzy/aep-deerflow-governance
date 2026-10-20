package v1alpha1

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestModelAccessCopies(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Employee model identity API copies")
}

var _ = Describe("Optional model identity cache isolation", func() {
	It("keeps absent identities nil", func() {
		var mapping *ModelAccessSpec
		var identity *ModelIdentityStatus
		Expect(mapping.DeepCopy()).To(BeNil())
		Expect(identity.DeepCopy()).To(BeNil())
		employee := &DigitalEmployee{}
		Expect(employee.DeepCopy().Spec.ModelAccess).To(BeNil())
		Expect(employee.DeepCopy().Status.ModelIdentity).To(BeNil())
	})

	It("does not share explicit mapping or observed identity across cached employee copies", func() {
		employee := &DigitalEmployee{
			Spec: DigitalEmployeeSpec{ModelAccess: &ModelAccessSpec{AgentID: "employee-agent"}},
			Status: DigitalEmployeeStatus{ModelIdentity: &ModelIdentityStatus{
				AgentID: "employee-agent", EmployeeUID: "employee-uid", ServiceAccountUID: "first-sa",
			}},
		}
		copy := employee.DeepCopy()
		copy.Spec.ModelAccess.AgentID = "another-agent"
		copy.Status.ModelIdentity.ServiceAccountUID = "replacement-sa"
		Expect(employee.Spec.ModelAccess.AgentID).To(Equal("employee-agent"))
		Expect(string(employee.Status.ModelIdentity.ServiceAccountUID)).To(Equal("first-sa"))
		Expect(employee.Spec.ModelAccess.DeepCopy()).To(Equal(employee.Spec.ModelAccess))
		Expect(employee.Status.ModelIdentity.DeepCopy()).To(Equal(employee.Status.ModelIdentity))
		mapping := &ModelAccessSpec{}
		employee.Spec.ModelAccess.DeepCopyInto(mapping)
		mapping.AgentID = "different-agent"
		identity := &ModelIdentityStatus{}
		employee.Status.ModelIdentity.DeepCopyInto(identity)
		identity.ServiceAccountUID = "different-sa"
		Expect(employee.Spec.ModelAccess.AgentID).NotTo(Equal(mapping.AgentID))
		Expect(employee.Status.ModelIdentity.ServiceAccountUID).NotTo(Equal(identity.ServiceAccountUID))
		status := employee.Status.DeepCopy()
		status.ModelIdentity = nil
		Expect(employee.Status.ModelIdentity).NotTo(BeNil())
	})
})
