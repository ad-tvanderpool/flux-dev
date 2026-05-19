/*
Copyright 2026 Travis Vanderpool

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

// Package controller_test exercises the ManifestGenerator reconciler
// end-to-end against an envtest-backed control plane plus a fake
// source-controller HTTP artifact server. The package is a deliberate
// black-box sibling of internal/controller; keeping it separate avoids
// import cycles with controller internals during integration tests.
package controller_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gotkconfig "github.com/fluxcd/pkg/artifact/config"
	gotkdigest "github.com/fluxcd/pkg/artifact/digest"
	gotkstorage "github.com/fluxcd/pkg/artifact/storage"
	gotktestenv "github.com/fluxcd/pkg/runtime/testenv"
	gotktestsrv "github.com/fluxcd/pkg/testserver"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
	"github.com/tvanderpool/flux-manifest-generator/internal/controller"
	// +kubebuilder:scaffold:imports
)

var (
	controllerName   = "flux-manifest-generator"
	timeout          = 30 * time.Second
	testStorage      *gotkstorage.Storage
	testServer       *gotktestsrv.ArtifactServer
	testEnv          *gotktestenv.Environment
	testClient       client.Client
	testCtx          = ctrl.SetupSignalHandler()
	retentionTTL     = 2 * time.Second
	retentionRecords = 2
)

func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(sourcev1.AddToScheme(s))
	utilruntime.Must(mgapi.AddToScheme(s))
	return s
}

func TestMain(m *testing.M) {
	var err error

	testEnv = gotktestenv.New(
		gotktestenv.WithCRDPath(
			filepath.Join("..", "..", "config", "crd", "bases"),
		),
		gotktestenv.WithScheme(newTestScheme()),
	)

	testServer, err = gotktestsrv.NewTempArtifactServer()
	if err != nil {
		panic(fmt.Sprintf("Failed to create a temporary storage server: %v", err))
	}
	testServer.Start()

	testStorage, err = newTestStorage(testServer.HTTPServer)
	if err != nil {
		panic(fmt.Sprintf("Failed to create a test storage: %v", err))
	}

	testClient, err = client.New(testEnv.Config, client.Options{Scheme: newTestScheme(), Cache: nil})
	if err != nil {
		panic(fmt.Sprintf("Failed to create test environment client: %v", err))
	}

	if err = registerController(); err != nil {
		panic(fmt.Sprintf("Failed to register controller: %v", err))
	}

	go func() {
		if err := testEnv.Start(testCtx); err != nil {
			panic(fmt.Sprintf("Failed to start test environment: %v", err))
		}
	}()
	<-testEnv.Manager.Elected()

	code := m.Run()

	if err := testEnv.Stop(); err != nil {
		panic(fmt.Sprintf("Failed to stop test environment: %v", err))
	}
	testServer.Stop()
	if err := os.RemoveAll(testServer.Root()); err != nil {
		panic(fmt.Sprintf("Failed to remove storage server dir: %v", err))
	}

	os.Exit(code)
}

func newTestStorage(s *gotktestsrv.HTTPServer) (*gotkstorage.Storage, error) {
	opts := &gotkconfig.Options{
		StoragePath:              s.Root(),
		StorageAddress:           s.URL(),
		StorageAdvAddress:        s.URL(),
		ArtifactRetentionTTL:     retentionTTL,
		ArtifactRetentionRecords: retentionRecords,
		ArtifactDigestAlgo:       gotkdigest.Canonical.String(),
	}
	return gotkstorage.New(opts)
}

func registerController() error {
	reconciler := &controller.ManifestGeneratorReconciler{
		ControllerName:            controllerName,
		Client:                    testEnv.Manager.GetClient(),
		APIReader:                 testEnv.Manager.GetAPIReader(),
		Scheme:                    testEnv.Scheme(),
		EventRecorder:             testEnv.GetEventRecorderFor(controllerName),
		Storage:                   testStorage,
		ArtifactFetchRetries:      1,
		DependencyRequeueInterval: 2 * time.Second,
	}
	return reconciler.SetupWithManager(testCtx, testEnv, controller.ManifestGeneratorReconcilerOptions{})
}
