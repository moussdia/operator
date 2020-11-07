/*
Copyright AppsCode Inc.

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

package kubernetes

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"go.bytebuilders.dev/license-verifier/info"
	"go.bytebuilders.dev/license-verifier/kubernetes/apis/licenses/v1alpha1"

	verifier "go.bytebuilders.dev/license-verifier"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server/mux"
	"k8s.io/client-go/kubernetes"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/reference"
	"k8s.io/klog"
	"k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
	core_util "kmodules.xyz/client-go/core/v1"
	"kmodules.xyz/client-go/dynamic"
	"kmodules.xyz/client-go/meta"
	"kmodules.xyz/client-go/tools/clusterid"
)

const (
	EventSourceLicenseVerifier           = "License Verifier"
	EventReasonLicenseVerificationFailed = "License Verification Failed"

	licensePath          = "/appscode/license"
	licenseCheckInterval = 1 * time.Hour
)

type LicenseEnforcer struct {
	opts        *verifier.Options
	config      *rest.Config
	k8sClient   kubernetes.Interface
	licenseFile string
}

// NewLicenseEnforcer returns a newly created license enforcer
func NewLicenseEnforcer(config *rest.Config, licenseFile string) *LicenseEnforcer {
	return &LicenseEnforcer{
		licenseFile: licenseFile,
		config:      config,
		opts: &verifier.Options{
			CACert:      []byte(info.LicenseCA),
			ProductName: info.ProductName,
		},
	}
}

func (le *LicenseEnforcer) createClients() (err error) {
	if le.k8sClient == nil {
		le.k8sClient, err = kubernetes.NewForConfig(le.config)
	}
	return err
}

func (le *LicenseEnforcer) readLicenseFromFile() (err error) {
	le.opts.License, err = ioutil.ReadFile(le.licenseFile)
	return err
}

func (le *LicenseEnforcer) readClusterUID() (err error) {
	le.opts.ClusterUID, err = clusterid.ClusterUID(le.k8sClient.CoreV1().Namespaces())
	return err
}

func (le *LicenseEnforcer) podName() (string, error) {
	if name, ok := os.LookupEnv("MY_POD_NAME"); ok {
		return name, nil
	}

	if meta.PossiblyInCluster() {
		// Read current pod name
		return os.Hostname()
	}
	return "", errors.New("failed to detect pod name")
}

func (le *LicenseEnforcer) handleLicenseVerificationFailure(licenseErr error) error {
	// Send interrupt so that all go-routines shut-down gracefully
	//nolint:errcheck
	defer syscall.Kill(syscall.Getpid(), syscall.SIGINT)

	// Log licenseInfo verification failure
	klog.Errorln("Failed to verify license. Reason: ", licenseErr.Error())

	podName, err := le.podName()
	if err != nil {
		return err
	}
	// Read the namespace of current pod
	namespace := meta.Namespace()

	// Find the root owner of this pod
	owner, _, err := dynamic.DetectWorkload(
		context.TODO(),
		le.config,
		core.SchemeGroupVersion.WithResource(core.ResourcePods.String()),
		namespace,
		podName,
	)
	if err != nil {
		return err
	}
	ref, err := reference.GetReference(clientscheme.Scheme, owner)
	if err != nil {
		return err
	}
	eventMeta := metav1.ObjectMeta{
		Name:      meta.NameWithSuffix(owner.GetName(), "license"),
		Namespace: namespace,
	}
	// Create an event against the root owner specifying that the license verification failed
	_, _, err = core_util.CreateOrPatchEvent(context.TODO(), le.k8sClient, eventMeta, func(in *core.Event) *core.Event {
		in.InvolvedObject = *ref
		in.Type = core.EventTypeWarning
		in.Source = core.EventSource{Component: EventSourceLicenseVerifier}
		in.Reason = EventReasonLicenseVerificationFailed
		in.Message = fmt.Sprintf("Failed to verify license. Reason: %s", licenseErr.Error())

		if in.FirstTimestamp.IsZero() {
			in.FirstTimestamp = metav1.Now()
		}
		in.LastTimestamp = metav1.Now()
		in.Count = in.Count + 1

		return in
	}, metav1.PatchOptions{})
	return err
}

// Install adds the License info handler
func (le *LicenseEnforcer) Install(c *mux.PathRecorderMux) {
	// Create Kubernetes client
	err := le.createClients()
	if err != nil {
		klog.Fatal(err)
		return
	}
	c.Handle(licensePath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-content-type-options", "nosniff")

		utilruntime.Must(json.NewEncoder(w).Encode(le.LoadLicense()))
	}))
}

func (le *LicenseEnforcer) LoadLicense() v1alpha1.License {
	utilruntime.Must(le.createClients())

	var license v1alpha1.License
	license.TypeMeta = metav1.TypeMeta{
		APIVersion: v1alpha1.SchemeGroupVersion.String(),
		Kind:       meta.GetKind(license),
	}

	// Read cluster UID (UID of the "kube-system" namespace)
	err := le.readClusterUID()
	if err != nil {
		license.Status = v1alpha1.LicenseUnknown
		license.Reason = err.Error()
		return license
	}
	// Read license from file
	err = le.readLicenseFromFile()
	if err != nil {
		license.Status = v1alpha1.LicenseUnknown
		license.Reason = err.Error()
		return license
	}
	// Parse license

	block, _ := pem.Decode(le.opts.License)
	if block == nil {
		// This probably is a JWT token, should be check for that when ready
		license.Status = v1alpha1.LicenseUnknown
		license.Reason = "failed to parse certificate PEM"
		return license
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		license.Status = v1alpha1.LicenseUnknown
		license.Reason = "failed to parse certificate, reason:" + err.Error()
		return license
	}

	license = v1alpha1.License{
		Issuer:    "byte.builders",
		Clusters:  cert.DNSNames,
		NotBefore: &metav1.Time{Time: cert.NotBefore},
		NotAfter:  &metav1.Time{Time: cert.NotAfter},
		ID:        cert.SerialNumber.String(),
		Products:  cert.Subject.Organization,
	}
	// ref: https://github.com/appscode/gitea/blob/master/models/stripe_license.go#L117-L126
	if err = verifier.VerifyLicense(le.opts); err != nil {
		license.Status = v1alpha1.LicenseExpired
		license.Reason = err.Error()
	} else {
		license.Status = v1alpha1.LicenseActive
	}
	return license
}

// VerifyLicensePeriodically periodically verifies whether the provided license is valid for the current cluster or not.
func VerifyLicensePeriodically(config *rest.Config, licenseFile string, stopCh <-chan struct{}) error {
	if info.SkipLicenseVerification() {
		klog.Infoln("License verification skipped")
		return nil
	}

	le := &LicenseEnforcer{
		licenseFile: licenseFile,
		config:      config,
		opts: &verifier.Options{
			CACert:      []byte(info.LicenseCA),
			ProductName: info.ProductName,
		},
	}
	// Create Kubernetes client
	err := le.createClients()
	if err != nil {
		return le.handleLicenseVerificationFailure(err)
	}
	// Read cluster UID (UID of the "kube-system" namespace)
	err = le.readClusterUID()
	if err != nil {
		return le.handleLicenseVerificationFailure(err)
	}

	// Periodically verify license with 1 hour interval
	fn := func() (done bool, err error) {
		klog.V(8).Infoln("Verifying license.......")
		// Read license from file
		err = le.readLicenseFromFile()
		if err != nil {
			return false, le.handleLicenseVerificationFailure(err)
		}
		// Validate license
		err = verifier.VerifyLicense(le.opts)
		if err != nil {
			return false, le.handleLicenseVerificationFailure(err)
		}
		klog.Infoln("Successfully verified license!")
		// return false so that the loop never ends
		return false, nil
	}

	if !info.EnforceLicenseImmediately() {
		licenseMissing := licenseFile == ""
		if _, err := os.Stat(licenseFile); os.IsNotExist(err) {
			licenseMissing = true
		}
		if licenseMissing {
			klog.Warningf("license file is missing. You have %v to acquire a valid license", licenseCheckInterval)

			return wait.PollUntil(licenseCheckInterval, fn, stopCh)
		}
	}

	return wait.PollImmediateUntil(licenseCheckInterval, fn, stopCh)
}

// CheckLicenseFile verifies whether the provided license is valid for the current cluster or not.
func CheckLicenseFile(config *rest.Config, licenseFile string) error {
	if info.SkipLicenseVerification() {
		klog.Infoln("License verification skipped")
		return nil
	}

	klog.V(8).Infoln("Verifying license.......")
	le := &LicenseEnforcer{
		licenseFile: licenseFile,
		config:      config,
		opts: &verifier.Options{
			CACert:      []byte(info.LicenseCA),
			ProductName: info.ProductName,
		},
	}
	// Create Kubernetes client
	err := le.createClients()
	if err != nil {
		return le.handleLicenseVerificationFailure(err)
	}
	// Read cluster UID (UID of the "kube-system" namespace)
	err = le.readClusterUID()
	if err != nil {
		return le.handleLicenseVerificationFailure(err)
	}
	// Read license from file
	err = le.readLicenseFromFile()
	if err != nil {
		return le.handleLicenseVerificationFailure(err)
	}
	// Validate license
	err = verifier.VerifyLicense(le.opts)
	if err != nil {
		return le.handleLicenseVerificationFailure(err)
	}
	klog.Infoln("Successfully verified license!")
	return nil
}

// CheckLicenseEndpoint verifies whether the provided api server has a valid license is valid for products.
func CheckLicenseEndpoint(config *rest.Config, apiServiceName string, products []string) error {
	aggrClient, err := clientset.NewForConfig(config)
	if err != nil {
		return err
	}

	apiSvc, err := aggrClient.ApiregistrationV1beta1().APIServices().Get(context.TODO(), apiServiceName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	c2 := *config
	c2.CAData = apiSvc.Spec.CABundle
	c2.Insecure = apiSvc.Spec.InsecureSkipTLSVerify
	rt, err := rest.TransportFor(&c2)
	if err != nil {
		return err
	}
	hc := http.Client{
		Transport: rt,
		Timeout:   30 * time.Second,
	}

	u, err := url.Parse(fmt.Sprintf("https://%s.%s.svc", apiSvc.Spec.Service.Name, apiSvc.Spec.Service.Namespace))
	if err != nil {
		return err
	}
	u.Path = licensePath

	resp, err := hc.Get(u.String())
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var license v1alpha1.License
	err = json.Unmarshal(data, &license)
	if err != nil {
		return err
	}

	if license.Status != v1alpha1.LicenseActive {
		return fmt.Errorf("license %s is not active, status: %s, reason: %s", license.ID, license.Status, license.Reason)
	}

	if !sets.NewString(license.Products...).HasAny(products...) {
		return fmt.Errorf("license %s is not valid for products %q", license.ID, strings.Join(products, ","))
	}
	return nil
}
