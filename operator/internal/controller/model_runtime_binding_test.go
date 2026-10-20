package controller

import (
	"encoding/json"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

var _ = Describe("Employee native model runtime binding contract", func() {
	It("matches pinned vectors from the actual AEP gatewayaccess BindingKey implementation", func() {
		var vectors []struct {
			Name, DeploymentID, AgentID, Namespace, EmployeeUID, ServiceAccountName, ServiceAccountUID, Audience, BindingKey string
		}
		raw, err := os.ReadFile("testdata/model-access-binding-v1.json")
		Expect(err).NotTo(HaveOccurred())
		Expect(json.Unmarshal(raw, &vectors)).To(Succeed())
		Expect(vectors).To(HaveLen(2))
		for _, v := range vectors {
			employee := &v1alpha1.DigitalEmployee{
				ObjectMeta: metav1.ObjectMeta{Namespace: v.Namespace, UID: types.UID(v.EmployeeUID)},
				Status: v1alpha1.DigitalEmployeeStatus{ModelIdentity: &v1alpha1.ModelIdentityStatus{
					DeploymentID: v.DeploymentID, AgentID: v.AgentID, EmployeeUID: types.UID(v.EmployeeUID),
					ServiceAccountName: v.ServiceAccountName, ServiceAccountUID: types.UID(v.ServiceAccountUID), Audience: v.Audience,
				}},
			}
			Expect(modelRuntimeBinding(employee)).To(Equal(v.BindingKey), v.Name)
		}
	})
})
