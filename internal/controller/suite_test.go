package controller

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	infrav1 "github.com/vpsieinc/cluster-api-provider-vpsie/api/v1alpha1"

	optv1 "github.com/vpsieinc/vpsie-cluster-scaler/api/v1alpha1"
)

var (
	testEnv    *envtest.Environment
	k8sClient  client.Client
	ctx        context.Context
	cancel     context.CancelFunc
	testScheme *runtime.Scheme
)

func TestMain(m *testing.M) {
	testScheme = runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(testScheme)
	_ = clusterv1.AddToScheme(testScheme)
	_ = infrav1.AddToScheme(testScheme)
	_ = optv1.AddToScheme(testScheme)

	// Locate CAPI CRDs from the Go module cache.
	capiCRDPath := findCAPICRDPath()

	// Locate CAPV CRDs from the local replace-linked module.
	capvCRDPath := findCAPVCRDPath()

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			capiCRDPath,
			capvCRDPath,
		},
		Scheme: testScheme,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		panic("failed to start envtest: " + err.Error())
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		panic("failed to create k8s client: " + err.Error())
	}

	ctx, cancel = context.WithCancel(context.Background())

	code := m.Run()

	cancel()
	if err := testEnv.Stop(); err != nil {
		panic("failed to stop envtest: " + err.Error())
	}
	os.Exit(code)
}

func findCAPICRDPath() string {
	out, err := exec.Command("go", "list", "-m", "-json", "sigs.k8s.io/cluster-api").CombinedOutput()
	if err != nil {
		panic("failed to resolve CAPI module path: " + string(out))
	}
	var modInfo struct{ Dir string }
	if err := json.Unmarshal(out, &modInfo); err != nil {
		panic("failed to parse CAPI module info: " + err.Error())
	}
	return filepath.Join(modInfo.Dir, "config", "crd", "bases")
}

func findCAPVCRDPath() string {
	// First try the replace-linked local directory (for development).
	localPath := filepath.Join("..", "..", "..", "cluster-api-provider-vpsie-2", "config", "crd", "bases")
	if _, err := os.Stat(localPath); err == nil {
		return localPath
	}
	// Fall back to resolving the module directory via go list (for CI).
	out, err := exec.Command("go", "list", "-m", "-json", "github.com/vpsieinc/cluster-api-provider-vpsie").CombinedOutput()
	if err != nil {
		panic("failed to resolve CAPV module path: " + string(out))
	}
	var modInfo struct{ Dir string }
	if err := json.Unmarshal(out, &modInfo); err != nil {
		panic("failed to parse module info: " + err.Error())
	}
	crdPath := filepath.Join(modInfo.Dir, "config", "crd", "bases")
	if _, err := os.Stat(crdPath); err != nil {
		panic("CAPV CRDs not found at " + crdPath + ": " + err.Error())
	}
	return crdPath
}
