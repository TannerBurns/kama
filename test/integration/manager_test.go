//go:build integration

/*
Copyright 2026 Kama Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestManagerStartsAndStopsCleanly(t *testing.T) {
	environment := &envtest.Environment{}
	restConfig, err := environment.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})

	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	config := clientcmdapi.NewConfig()
	config.Clusters["envtest"] = &clientcmdapi.Cluster{
		Server:                   restConfig.Host,
		CertificateAuthorityData: restConfig.CAData,
	}
	config.AuthInfos["envtest"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: restConfig.CertData,
		ClientKeyData:         restConfig.KeyData,
	}
	config.Contexts["envtest"] = &clientcmdapi.Context{Cluster: "envtest", AuthInfo: "envtest"}
	config.CurrentContext = "envtest"
	data, err := clientcmd.Write(*config)
	if err != nil {
		t.Fatalf("serialize kubeconfig: %v", err)
	}
	if err := os.WriteFile(kubeconfig, data, 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	probeAddress := reserveAddress(t)
	binary := os.Getenv("KAMA_MANAGER_BINARY")
	if binary == "" {
		binary = filepath.Join("..", "..", "bin", "manager")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary,
		"--metrics-bind-address=0",
		"--health-probe-bind-address="+probeAddress,
	)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}

	waitForHealthy(t, ctx, "http://"+probeAddress+"/healthz", &output)
	waitForHealthy(t, ctx, "http://"+probeAddress+"/readyz", &output)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal manager: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("manager exited with error: %v\n%s", err, output.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("manager did not stop cleanly\n%s", output.String())
	}
}

func reserveAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve probe port: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release probe port: %v", err)
	}
	return address
}

func waitForHealthy(t *testing.T, ctx context.Context, url string, output *bytes.Buffer) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("build probe request: %v", err)
		}
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal(fmt.Sprintf("manager probe %s did not become healthy: %v\n%s", url, ctx.Err(), output.String()))
		case <-ticker.C:
		}
	}
}
