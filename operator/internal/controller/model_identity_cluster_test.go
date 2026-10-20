package controller

import (
	"encoding/json"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("Employee workload identity Kubernetes API", func() {
	It("mounts a bound identity and rejects wrong audiences and a retired SA UID", func(ctx SpecContext) {
		kubeconfig := os.Getenv("EMPLOYEE_IDENTITY_KUBECONFIG")
		if kubeconfig == "" {
			Skip("requires an explicitly selected disposable Kubernetes cluster")
		}
		config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		Expect(err).NotTo(HaveOccurred())
		api, err := kubernetes.NewForConfig(config)
		Expect(err).NotTo(HaveOccurred())
		fx := newFixture(ctx)
		DeferCleanup(func() { fx.server.Close(); fx.runtimeSrv.Close() })
		real, err := client.New(config, client.Options{Scheme: fx.client.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		ns, err := api.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "employee-identity-"}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(cleanupCtx SpecContext) {
			Expect(api.CoreV1().Namespaces().Delete(cleanupCtx, ns.Name, metav1.DeleteOptions{})).To(Succeed())
		})
		employee := fx.employeeFixture("identity-probe")
		employee.Namespace, employee.Spec.Memory, employee.Spec.A2A = ns.Name, nil, nil
		Expect(real.Create(ctx, employee)).To(Succeed())
		r := &DigitalEmployeeReconciler{Client: real, Scheme: real.Scheme(), ModelTokenAudience: "urn:zhiyuan:model-gateway:test"}
		deployment, err := r.renderDeployment(employee, "cluster-probe")
		Expect(err).NotTo(HaveOccurred())
		Expect(r.prepareModelIdentity(ctx, employee, deployment)).To(Succeed())
		accountName := deployment.Spec.Template.Spec.ServiceAccountName
		sa, err := api.CoreV1().ServiceAccounts(ns.Name).Get(ctx, accountName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		// This minimal probe consumes the actual projection produced by the
		// operator. It does not claim a DeerFlow/Authorino/model request test.
		var identityVolume corev1.Volume
		for _, volume := range deployment.Spec.Template.Spec.Volumes {
			if volume.Name == modelTokenVolume {
				identityVolume = volume
			}
		}
		const script = `const fs=require('fs');const c=JSON.parse(Buffer.from(fs.readFileSync(process.env.ZHIYUAN_MODEL_TOKEN_FILE,'utf8').split('.')[1],'base64url'));console.log(JSON.stringify({aud:c.aud,sub:c.sub,sa:c['kubernetes.io'].serviceaccount.uid,pod:c['kubernetes.io'].pod.uid,exp:c.exp,iat:c.iat}));setInterval(()=>{},1000);`
		pod, err := api.CoreV1().Pods(ns.Name).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "identity-probe"}, Spec: corev1.PodSpec{
			ServiceAccountName: accountName, AutomountServiceAccountToken: ptr.To(false), RestartPolicy: corev1.RestartPolicyNever,
			Volumes: []corev1.Volume{identityVolume}, Containers: []corev1.Container{{Name: "runtime", Image: "node:24-alpine", ImagePullPolicy: corev1.PullNever,
				Command: []string{"node", "-e", script}, Env: []corev1.EnvVar{{Name: "ZHIYUAN_MODEL_TOKEN_FILE", Value: modelTokenDirectory + "/token"}},
				VolumeMounts: []corev1.VolumeMount{{Name: modelTokenVolume, MountPath: modelTokenDirectory, ReadOnly: true}},
			}},
		}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		var facts struct {
			Aud          []string
			Sub, SA, Pod string
			Exp, Iat     int64
		}
		Eventually(func(g Gomega) {
			data, readErr := api.CoreV1().Pods(ns.Name).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "runtime"}).DoRaw(ctx)
			g.Expect(readErr).NotTo(HaveOccurred())
			g.Expect(json.Unmarshal(data, &facts)).To(Succeed())
		}, 90*time.Second, time.Second).Should(Succeed())
		Expect(facts.Aud).To(Equal([]string{r.ModelTokenAudience}))
		Expect(facts.Sub).To(Equal("system:serviceaccount:" + ns.Name + ":" + accountName))
		Expect(facts.SA).To(Equal(string(sa.UID)))
		Expect(facts.Pod).To(Equal(string(pod.UID)))
		Expect(facts.Exp).To(BeNumerically(">", facts.Iat))
		GinkgoWriter.Println("PASS: kubelet mounted the actual audience- and Pod-bound projection; no bearer logged")
		token, err := api.CoreV1().ServiceAccounts(ns.Name).CreateToken(ctx, accountName, &authv1.TokenRequest{Spec: authv1.TokenRequestSpec{
			Audiences: []string{r.ModelTokenAudience}, ExpirationSeconds: ptr.To(int64(600)),
			BoundObjectRef: &authv1.BoundObjectReference{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID},
		}}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		// The bearer remains in memory and is only sent to TokenReview. Never
		// include a token/review spec in an assertion or a test report.
		review := func(audience string) *authv1.TokenReviewStatus {
			result, reviewErr := api.AuthenticationV1().TokenReviews().Create(ctx, &authv1.TokenReview{Spec: authv1.TokenReviewSpec{
				Token: token.Status.Token, Audiences: []string{audience},
			}}, metav1.CreateOptions{})
			ExpectWithOffset(1, reviewErr).NotTo(HaveOccurred())
			return &result.Status
		}
		status := review(r.ModelTokenAudience)
		Expect(status.Authenticated).To(BeTrue())
		Expect(status.User.UID).To(Equal(string(sa.UID)))
		Expect(review("urn:zhiyuan:wrong-gateway").Authenticated).To(BeFalse())
		GinkgoWriter.Println("PASS: native TokenReview accepts the bound SA UID and denies a different audience")
		Expect(api.CoreV1().ServiceAccounts(ns.Name).Delete(ctx, accountName, metav1.DeleteOptions{})).To(Succeed())
		replacement, err := r.renderDeployment(employee, "cluster-probe")
		Expect(err).NotTo(HaveOccurred())
		Expect(r.prepareModelIdentity(ctx, employee, replacement)).To(Succeed())
		Expect(replacement.Spec.Template.Spec.ServiceAccountName).To(Equal(accountName))
		Expect(replacement.Spec.Template.Annotations[modelAccountUIDAnnotation]).NotTo(Equal(string(sa.UID)))
		Eventually(func() bool { return review(r.ModelTokenAudience).Authenticated }, 90*time.Second, time.Second).Should(BeFalse())
		GinkgoWriter.Println("PASS: same-name SA recreation changes the rollout UID and old bound credentials are rejected")
	}, SpecTimeout(4*time.Minute))
})
