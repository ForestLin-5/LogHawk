package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// ===== TYPES =====

type Scenario struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Active       bool     `json:"active"`
	Fixed        bool     `json:"fixed"`
	StartedAt    string   `json:"started_at,omitempty"`
	AutoRecover  int      `json:"auto_recover_sec"` // seconds until auto-recovery
	Hints        []string `json:"hints"`
}

type ChaosStatus struct {
	Scenarios []Scenario `json:"scenarios"`
	ActiveID  string     `json:"active_id,omitempty"`
}

type VerifyResult struct {
	Passed  bool   `json:"passed"`
	Message string `json:"message"`
}

var (
	namespace  = envOr("CHAOS_NAMESPACE", "loghawk")
	port       = envOr("CHAOS_PORT", "8005")
	apiToken   = os.Getenv("CHAOS_API_TOKEN")
	clientset  *kubernetes.Clientset
	mu         sync.Mutex
	activeID   string
	autoTimer  *time.Timer
	startTimes = make(map[string]time.Time)
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next(w, r)
			return
		}
		if apiToken == "" {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": "CHAOS_API_TOKEN not configured"})
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got != apiToken {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func k8sCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

var scenarios = []Scenario{
	{
		ID:          "pod-deleted",
		Name:        "Pod Mysteriously Vanished",
		Description: "Ingest service pod keeps CrashLoopBackOff, log ingestion interrupted",
		Hints: []string{
			"Hint: Check Ingest Pod status: kubectl -n loghawk describe pod -l app=ingest",
			"Hint: Look at Events for errors -- is the image failing to pull?",
			"Hint: The image tag was changed to a non-existent version. Fix: kubectl -n loghawk set image deploy/ingest ingest=loghawk/ingest:v1.0.0",
		},
	},
	{
		ID:          "service-broken",
		Name:        "Traffic Not Coming In",
		Description: "Frontend loads but shows no data, all API calls timeout",
		Hints: []string{
			"Hint: Check Service Endpoints: kubectl -n loghawk get endpoints",
			"Hint: Does the Ingest Service selector match the Pod labels?",
			"Hint: The selector was changed incorrectly. Fix: kubectl -n loghawk edit svc ingest, change selector back to app: ingest",
		},
	},
	{
		ID:          "config-messed",
		Name:        "Config Got Tampered With",
		Description: "Ingest logs show cannot connect to message queue, all ingestion failing",
		Hints: []string{
			"Hint: Check Ingest environment variables: kubectl -n loghawk describe pod -l app=ingest | grep -A20 Environment",
			"Hint: The ConfigMap values may be wrong: kubectl -n loghawk get configmap loghawk-config -o yaml",
			"Hint: RABBITMQ_HOST was changed to a wrong value. Fix: kubectl -n loghawk edit configmap loghawk-config, change back to rabbitmq.loghawk",
		},
	},
	{
		ID:          "scale-zero",
		Name:        "Who Scaled Replicas to Zero?",
		Description: "Ingest service completely unavailable, frontend log stream frozen",
		Hints: []string{
			"Hint: Check Deployment replicas: kubectl -n loghawk get deploy",
			"Hint: Ingest READY shows 0/2? It was scaled down.",
			"Hint: Fix: kubectl -n loghawk scale deploy ingest --replicas=2",
		},
	},
	{
		ID:          "network-blocked",
		Name:        "Network Is Blocked",
		Description: "Services cannot reach each other, entire system paralyzed",
		Hints: []string{
			"Hint: Check NetworkPolicies: kubectl -n loghawk get networkpolicy",
			"Hint: There is a deny-all policy blocking all traffic.",
			"Hint: Fix: kubectl -n loghawk delete networkpolicy deny-all-chaos",
		},
	},
	{
		ID:          "disk-full",
		Name:        "Disk Is About to Explode",
		Description: "A node shows disk pressure, pods are being evicted",
		Hints: []string{
			"Hint: Check node status: kubectl get nodes and kubectl describe node | grep Taints",
			"Hint: See if there's a disk-filler pod running: kubectl -n loghawk get pods | grep disk-filler",
			"Hint: Fix: kubectl -n loghawk delete pod -l chaos=disk-filler, then wait a few minutes for the node to recover",
		},
	},
}

func main() {
	// Init K8s client
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("Failed to get in-cluster config: %v (are you running in K8s?)", err)
	}
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create clientset: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/chaos/break", authMiddleware(handleBreak))
	mux.HandleFunc("/chaos/verify", authMiddleware(handleVerify))
	mux.HandleFunc("/chaos/hint", authMiddleware(handleHint))
	mux.HandleFunc("/chaos/status", authMiddleware(handleStatus))
	mux.HandleFunc("/chaos/reset", authMiddleware(handleReset))
	mux.HandleFunc("/health", handleHealth)

	addr := ":" + port
	log.Printf("LogHawk Chaos Service starting on %s (namespace: %s)", addr, namespace)
	log.Printf("   POST /chaos/break?scenario=xxx  -- inject fault")
	log.Printf("   POST /chaos/verify?scenario=xxx -- verify fix")
	log.Printf("   GET  /chaos/hint?scenario=xxx&level=N -- get hint")
	log.Printf("   GET  /chaos/status              -- status")
	log.Printf("   POST /chaos/reset               -- reset all")

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down chaos...")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("Shutdown error: %v", err)
	}
	log.Println("chaos stopped")
}

// ===== HANDLERS =====

func handleHealth(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "active": activeID})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()

	var list []Scenario
	for _, s := range scenarios {
		sc := s
		if t, ok := startTimes[s.ID]; ok && sc.Active {
			elapsed := int(time.Since(t).Seconds())
			sc.AutoRecover = 300 - elapsed
			if sc.AutoRecover < 0 {
				sc.AutoRecover = 0
			}
		}
		if s.ID == activeID {
			sc.Active = true
		}
		list = append(list, sc)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ChaosStatus{Scenarios: list, ActiveID: activeID})
}

func handleHint(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("scenario")
	level := 1
	fmt.Sscanf(r.URL.Query().Get("level"), "%d", &level)

	var hints []string
	for _, s := range scenarios {
		if s.ID == sid {
			if level >= 1 && level <= len(s.Hints) {
				hints = append(hints, s.Hints[level-1])
			}
			break
		}
	}
	if hints == nil {
		hints = []string{"Unknown scenario"}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"scenario": sid, "level": level, "hints": hints})
}

func handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	mu.Lock()
	defer mu.Unlock()

	if autoTimer != nil {
		autoTimer.Stop()
		autoTimer = nil
	}
	activeID = ""
	startTimes = make(map[string]time.Time)

	// Reset all scenarios
	rctx, rcancel := k8sCtx()
	defer rcancel()
	for _, s := range scenarios {
		resetScenario(rctx, s.ID)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "reset", "message": "All scenarios have been reset"})
}

func handleBreak(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	sid := r.URL.Query().Get("scenario")

	mu.Lock()
	if activeID != "" && activeID != sid {
		mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("Active fault already exists: %s, please fix or reset first", activeID)})
		return
	}
	activeID = sid
	startTimes[sid] = time.Now()

	// Start auto-recovery timer (5 minutes)
	if autoTimer != nil {
		autoTimer.Stop()
	}
	autoTimer = time.AfterFunc(5*time.Minute, func() {
		mu.Lock()
		defer mu.Unlock()
		actx, acancel := k8sCtx()
		defer acancel()
		resetScenario(actx, sid)
		activeID = ""
		log.Printf("Auto-recovered: %s", sid)
	})
	mu.Unlock()

	ictx, icancel := k8sCtx()
	defer icancel()
	err := injectScenario(ictx, sid)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "injected", "scenario": sid, "auto_recover_sec": 300,
	})
}

func handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	sid := r.URL.Query().Get("scenario")

	mu.Lock()
	if sid != activeID {
		mu.Unlock()
		json.NewEncoder(w).Encode(VerifyResult{Passed: false, Message: "No active fault, nothing to fix"})
		return
	}
	mu.Unlock()

	vctx, vcancel := k8sCtx()
	defer vcancel()
	result := verifyScenario(vctx, sid)
	if result.Passed {
		mu.Lock()
		activeID = ""
		if autoTimer != nil {
			autoTimer.Stop()
			autoTimer = nil
		}
		delete(startTimes, sid)
		mu.Unlock()

		// Reset the scenario to clean state
		resetScenario(vctx, sid)
	}

	json.NewEncoder(w).Encode(result)
}

// ===== SCENARIO INJECTORS =====

func injectScenario(ctx context.Context, sid string) error {
	switch sid {
	case "pod-deleted":
		return injectPodCrash(ctx)
	case "service-broken":
		return injectServiceBroken(ctx)
	case "config-messed":
		return injectConfigMessed(ctx)
	case "scale-zero":
		return injectScaleZero(ctx)
	case "network-blocked":
		return injectNetworkBlock(ctx)
	case "disk-full":
		return injectDiskFull(ctx)
	default:
		return fmt.Errorf("unknown scenario: %s", sid)
	}
}

func verifyScenario(ctx context.Context, sid string) VerifyResult {
	switch sid {
	case "pod-deleted":
		return verifyPodCrash(ctx)
	case "service-broken":
		return verifyServiceBroken(ctx)
	case "config-messed":
		return verifyConfigMessed(ctx)
	case "scale-zero":
		return verifyScaleZero(ctx)
	case "network-blocked":
		return verifyNetworkBlock(ctx)
	case "disk-full":
		return verifyDiskFull(ctx)
	default:
		return VerifyResult{Passed: false, Message: "Unknown scenario"}
	}
}

func resetScenario(ctx context.Context, sid string) {
	switch sid {
	case "pod-deleted":
		resetPodCrash(ctx)
	case "service-broken":
		resetServiceBroken(ctx)
	case "config-messed":
		resetConfigMessed(ctx)
	case "scale-zero":
		resetScaleZero(ctx)
	case "network-blocked":
		resetNetworkBlock(ctx)
	case "disk-full":
		resetDiskFull(ctx)
	}
}

// ===== SCENARIO 1: Pod Crash (wrong image) =====

func injectPodCrash(ctx context.Context) error {
	deploy, err := clientset.AppsV1().Deployments(namespace).Get(ctx, "ingest", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get deploy: %w", err)
	}
	// Change image to non-existent tag
	for i := range deploy.Spec.Template.Spec.Containers {
		if deploy.Spec.Template.Spec.Containers[i].Name == "ingest" {
			deploy.Spec.Template.Spec.Containers[i].Image = "loghawk/ingest:broken-tag-does-not-exist"
		}
	}
	_, err = clientset.AppsV1().Deployments(namespace).Update(ctx, deploy, metav1.UpdateOptions{})
	return err
}

func verifyPodCrash(ctx context.Context) VerifyResult {
	deploy, err := clientset.AppsV1().Deployments(namespace).Get(ctx, "ingest", metav1.GetOptions{})
	if err != nil {
		return VerifyResult{Passed: false, Message: fmt.Sprintf("Failed to get Deployment: %v", err)}
	}
	for _, c := range deploy.Spec.Template.Spec.Containers {
		if c.Name == "ingest" && strings.Contains(c.Image, "broken") {
			return VerifyResult{Passed: false, Message: "Image is still broken, not fixed yet"}
		}
	}
	// Check pods are running
	pods, _ := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=ingest"})
	running := 0
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			running++
		}
	}
	if running == 0 {
		return VerifyResult{Passed: false, Message: "Pod is not running yet"}
	}
	return VerifyResult{Passed: true, Message: fmt.Sprintf("Fixed! %d pod(s) running", running)}
}

func resetPodCrash(ctx context.Context) {
	deploy, err := clientset.AppsV1().Deployments(namespace).Get(ctx, "ingest", metav1.GetOptions{})
	if err != nil {
		return
	}
	changed := false
	for i := range deploy.Spec.Template.Spec.Containers {
		if deploy.Spec.Template.Spec.Containers[i].Name == "ingest" && strings.Contains(deploy.Spec.Template.Spec.Containers[i].Image, "broken") {
			deploy.Spec.Template.Spec.Containers[i].Image = "loghawk/ingest:v1.0.0"
			changed = true
		}
	}
	if changed {
		clientset.AppsV1().Deployments(namespace).Update(ctx, deploy, metav1.UpdateOptions{})
	}
}

// ===== SCENARIO 2: Service Broken (wrong selector) =====

func injectServiceBroken(ctx context.Context) error {
	svc, err := clientset.CoreV1().Services(namespace).Get(ctx, "ingest", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get svc: %w", err)
	}
	svc.Spec.Selector = map[string]string{"app": "ingest-broken-not-exist"}
	_, err = clientset.CoreV1().Services(namespace).Update(ctx, svc, metav1.UpdateOptions{})
	return err
}

func verifyServiceBroken(ctx context.Context) VerifyResult {
	ep, err := clientset.CoreV1().Endpoints(namespace).Get(ctx, "ingest", metav1.GetOptions{})
	if err != nil {
		return VerifyResult{Passed: false, Message: fmt.Sprintf("Failed to get Endpoints: %v", err)}
	}
	if len(ep.Subsets) == 0 {
		return VerifyResult{Passed: false, Message: "Endpoints still empty, Service not connected to Pod"}
	}
	return VerifyResult{Passed: true, Message: "Fixed! Endpoints restored"}
}

func resetServiceBroken(ctx context.Context) {
	svc, err := clientset.CoreV1().Services(namespace).Get(ctx, "ingest", metav1.GetOptions{})
	if err != nil {
		return
	}
	if svc.Spec.Selector["app"] == "ingest-broken-not-exist" {
		svc.Spec.Selector = map[string]string{"app": "ingest"}
		clientset.CoreV1().Services(namespace).Update(ctx, svc, metav1.UpdateOptions{})
	}
}

// ===== SCENARIO 3: Config Messed =====

func injectConfigMessed(ctx context.Context) error {
	cm, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, "loghawk-config", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get configmap: %w", err)
	}
	cm.Data["RABBITMQ_HOST"] = "rabbitmq.broken-wrong-host"
	_, err = clientset.CoreV1().ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

func verifyConfigMessed(ctx context.Context) VerifyResult {
	cm, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, "loghawk-config", metav1.GetOptions{})
	if err != nil {
		return VerifyResult{Passed: false, Message: fmt.Sprintf("Failed to get ConfigMap: %v", err)}
	}
	if cm.Data["RABBITMQ_HOST"] == "rabbitmq.broken-wrong-host" {
		return VerifyResult{Passed: false, Message: "RABBITMQ_HOST is still wrong"}
	}
	return VerifyResult{Passed: true, Message: "Fixed! Config restored"}
}

func resetConfigMessed(ctx context.Context) {
	cm, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, "loghawk-config", metav1.GetOptions{})
	if err != nil {
		return
	}
	if cm.Data["RABBITMQ_HOST"] == "rabbitmq.broken-wrong-host" {
		cm.Data["RABBITMQ_HOST"] = "rabbitmq.loghawk"
		clientset.CoreV1().ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{})
	}
}

// ===== SCENARIO 4: Scale to Zero =====

func injectScaleZero(ctx context.Context) error {
	scale, err := clientset.AppsV1().Deployments(namespace).GetScale(ctx, "ingest", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get scale: %w", err)
	}
	scale.Spec.Replicas = 0
	_, err = clientset.AppsV1().Deployments(namespace).UpdateScale(ctx, "ingest", scale, metav1.UpdateOptions{})
	return err
}

func verifyScaleZero(ctx context.Context) VerifyResult {
	deploy, err := clientset.AppsV1().Deployments(namespace).Get(ctx, "ingest", metav1.GetOptions{})
	if err != nil {
		return VerifyResult{Passed: false, Message: fmt.Sprintf("Failed to get Deployment: %v", err)}
	}
	if *deploy.Spec.Replicas == 0 {
		return VerifyResult{Passed: false, Message: "Replicas still 0"}
	}
	pods, _ := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=ingest"})
	running := 0
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			running++
		}
	}
	if running == 0 {
		return VerifyResult{Passed: false, Message: "Pod not started yet"}
	}
	return VerifyResult{Passed: true, Message: fmt.Sprintf("Fixed! %d pod(s) running", running)}
}

func resetScaleZero(ctx context.Context) {
	deploy, err := clientset.AppsV1().Deployments(namespace).Get(ctx, "ingest", metav1.GetOptions{})
	if err != nil {
		return
	}
	if *deploy.Spec.Replicas == 0 {
		two := int32(2)
		deploy.Spec.Replicas = &two
		clientset.AppsV1().Deployments(namespace).Update(ctx, deploy, metav1.UpdateOptions{})
	}
}

// ===== SCENARIO 5: Network Blocked =====

func injectNetworkBlock(ctx context.Context) error {
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "deny-all-chaos", Namespace: namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
	_, err := clientset.NetworkingV1().NetworkPolicies(namespace).Create(ctx, policy, metav1.CreateOptions{})
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil // already injected
	}
	return err
}

func verifyNetworkBlock(ctx context.Context) VerifyResult {
	_, err := clientset.NetworkingV1().NetworkPolicies(namespace).Get(ctx, "deny-all-chaos", metav1.GetOptions{})
	if err == nil {
		return VerifyResult{Passed: false, Message: "deny-all-chaos policy still exists, not deleted"}
	}
	return VerifyResult{Passed: true, Message: "Fixed! Network policy deleted"}
}

func resetNetworkBlock(ctx context.Context) {
	clientset.NetworkingV1().NetworkPolicies(namespace).Delete(ctx, "deny-all-chaos", metav1.DeleteOptions{})
}

// ===== SCENARIO 6: Disk Full =====

func injectDiskFull(ctx context.Context) error {
	// Create a Pod that fills disk
	diskFiller := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "disk-filler-chaos",
			Namespace: namespace,
			Labels:    map[string]string{"chaos": "disk-filler"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   boolPtr(true),
				RunAsUser:      int64Ptr(65534),
				RunAsGroup:     int64Ptr(65534),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:    "filler",
				Image:   "busybox:1.36",
				Command: []string{"sh", "-c", "dd if=/dev/zero of=/tmp/bigfile bs=1M count=500; sleep 3600"},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceEphemeralStorage: resourceQuantity("600Mi")},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: boolPtr(false),
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
					},
					ReadOnlyRootFilesystem: boolPtr(true),
				},
			}},
		},
	}
	_, err := clientset.CoreV1().Pods(namespace).Create(ctx, diskFiller, metav1.CreateOptions{})
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

func verifyDiskFull(ctx context.Context) VerifyResult {
	_, err := clientset.CoreV1().Pods(namespace).Get(ctx, "disk-filler-chaos", metav1.GetOptions{})
	if err == nil {
		return VerifyResult{Passed: false, Message: "disk-filler is still running, delete it first"}
	}
	return VerifyResult{Passed: true, Message: "Fixed! Disk filler pod has been deleted"}
}

func resetDiskFull(ctx context.Context) {
	clientset.CoreV1().Pods(namespace).Delete(ctx, "disk-filler-chaos", metav1.DeleteOptions{})
}

func resourceQuantity(s string) resource.Quantity {
	q, _ := resource.ParseQuantity(s)
	return q
}

func boolPtr(b bool) *bool    { return &b }
func int64Ptr(i int64) *int64 { return &i }
