package fixture

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/argoproj/pkg/errors"
	jsonpatch "github.com/evanphx/json-patch"
	"github.com/ghodss/yaml"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/argoproj/argo-cd/common"
	. "github.com/argoproj/argo-cd/errors"
	argocdclient "github.com/argoproj/argo-cd/pkg/apiclient"
	sessionpkg "github.com/argoproj/argo-cd/pkg/apiclient/session"
	"github.com/argoproj/argo-cd/pkg/apis/application/v1alpha1"
	appclientset "github.com/argoproj/argo-cd/pkg/client/clientset/versioned"
	"github.com/argoproj/argo-cd/util"
	grpcutil "github.com/argoproj/argo-cd/util/grpc"
	"github.com/argoproj/argo-cd/util/rand"
	"github.com/argoproj/argo-cd/util/settings"
)

const (
	defaultAriServer = "localhost:8080"
	adminPassword    = "password"
	testingLabel     = "e2e.argoproj.io"
	ArgoCDNamespace  = "argocd-e2e"

	// ensure all repos are in one directory tree, so we can easily clean them up
	tmpDir = "/tmp/argo-e2e"
)

var (
	id               string
	name             string
	KubeClientset    kubernetes.Interface
	AppClientset     appclientset.Interface
	ArgoCDClientset  argocdclient.Client
	settingsManager  *settings.SettingsManager
	apiServerAddress string
	token            string
	plainText        bool
	repoUrl          string
)

// getKubeConfig creates new kubernetes client config using specified config path and config overrides variables
func getKubeConfig(configPath string, overrides clientcmd.ConfigOverrides) *rest.Config {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = configPath
	clientConfig := clientcmd.NewInteractiveDeferredLoadingClientConfig(loadingRules, &overrides, os.Stdin)

	restConfig, err := clientConfig.ClientConfig()
	CheckError(err)
	return restConfig
}

// creates e2e tests fixture: ensures that Application CRD is installed, creates temporal namespace, starts repo and api server,
// configure currently available cluster.
func init() {
	// set-up variables
	config := getKubeConfig("", clientcmd.ConfigOverrides{})
	AppClientset = appclientset.NewForConfigOrDie(config)
	KubeClientset = kubernetes.NewForConfigOrDie(config)
	apiServerAddress = os.Getenv(argocdclient.EnvArgoCDServer)
	if apiServerAddress == "" {
		apiServerAddress = defaultAriServer
	}
	tlsTestResult, err := grpcutil.TestTLS(apiServerAddress)
	CheckError(err)

	ArgoCDClientset, err = argocdclient.NewClient(&argocdclient.ClientOptions{Insecure: true, ServerAddr: apiServerAddress, PlainText: !tlsTestResult.TLS})
	CheckError(err)

	closer, client, err := ArgoCDClientset.NewSessionClient()
	CheckError(err)
	defer util.Close(closer)

	sessionResponse, err := client.Create(context.Background(), &sessionpkg.SessionCreateRequest{Username: "admin", Password: adminPassword})
	CheckError(err)

	ArgoCDClientset, err = argocdclient.NewClient(&argocdclient.ClientOptions{
		Insecure:   true,
		ServerAddr: apiServerAddress,
		AuthToken:  sessionResponse.Token,
		PlainText:  !tlsTestResult.TLS,
	})
	CheckError(err)

	settingsManager = settings.NewSettingsManager(context.Background(), KubeClientset, "argocd-e2e")
	token = sessionResponse.Token
	plainText = !tlsTestResult.TLS

	log.WithFields(log.Fields{"apiServerAddress": apiServerAddress}).Info("initialized")
}

func Name() string {
	return name
}

func repoDirectory() string {
	return path.Join(tmpDir, name)
}
func SetRepoURL(url string) {
	repoUrl = url
}

func RepoURL() string {
	return repoUrl
}

func DeploymentNamespace() string {
	return dnsFriendly(fmt.Sprintf("argocd-e2e-%s", id))
}

// creates a secret for the current test, this currently can only create a single secret
func CreateSecret(username, password string) string {
	secretName := fmt.Sprintf("argocd-e2e-%s", name)
	FailOnErr(Run("", "kubectl", "create", "secret", "generic", secretName,
		"--from-literal=username="+username,
		"--from-literal=password="+password,
		"-n", ArgoCDNamespace))
	FailOnErr(Run("", "kubectl", "label", "secret", secretName, testingLabel+"=true", "-n", ArgoCDNamespace))
	return secretName
}

func Settings(consumer func(s *settings.ArgoCDSettings)) {
	s, err := settingsManager.GetSettings()
	CheckError(err)
	consumer(s)
	CheckError(settingsManager.SaveSettings(s))
}

func updateSettingConfigMap(updater func(cm *corev1.ConfigMap) error) {
	cm, err := KubeClientset.CoreV1().ConfigMaps(ArgoCDNamespace).Get(common.ArgoCDConfigMapName, v1.GetOptions{})
	errors.CheckError(err)
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	errors.CheckError(updater(cm))
	_, err = KubeClientset.CoreV1().ConfigMaps(ArgoCDNamespace).Update(cm)
	errors.CheckError(err)
}

func SetResourceOverrides(overrides map[string]v1alpha1.ResourceOverride) {
	updateSettingConfigMap(func(cm *corev1.ConfigMap) error {
		if len(overrides) > 0 {
			yamlBytes, err := yaml.Marshal(overrides)
			if err != nil {
				return err
			}
			cm.Data["resource.customizations"] = string(yamlBytes)
		} else {
			delete(cm.Data, "resource.customizations")
		}
		return nil
	})
}

func SetConfigManagementPlugins(plugin ...v1alpha1.ConfigManagementPlugin) {
	updateSettingConfigMap(func(cm *corev1.ConfigMap) error {
		yamlBytes, err := yaml.Marshal(plugin)
		if err != nil {
			return err
		}
		cm.Data["configManagementPlugins"] = string(yamlBytes)
		return nil
	})
}

func SetHelmRepoCredential(creds settings.HelmRepoCredentials) {
	Settings(func(s *settings.ArgoCDSettings) {
		s.HelmRepositories = []settings.HelmRepoCredentials{creds}
	})
}

func EnsureCleanState(t *testing.T) {

	start := time.Now()

	policy := v1.DeletePropagationBackground
	// delete resources
	// kubectl delete apps --all
	CheckError(AppClientset.ArgoprojV1alpha1().Applications(ArgoCDNamespace).DeleteCollection(&v1.DeleteOptions{PropagationPolicy: &policy}, v1.ListOptions{}))
	// kubectl delete appprojects --field-selector metadata.name!=default
	CheckError(AppClientset.ArgoprojV1alpha1().AppProjects(ArgoCDNamespace).DeleteCollection(
		&v1.DeleteOptions{PropagationPolicy: &policy}, v1.ListOptions{FieldSelector: "metadata.name!=default"}))
	// kubectl delete secrets -l e2e.argoproj.io=true
	CheckError(KubeClientset.CoreV1().Secrets(ArgoCDNamespace).DeleteCollection(
		&v1.DeleteOptions{PropagationPolicy: &policy}, v1.ListOptions{LabelSelector: testingLabel + "=true"}))

	FailOnErr(Run("", "kubectl", "delete", "ns", "-l", testingLabel+"=true", "--field-selector", "status.phase=Active", "--wait=false"))

	// reset settings
	s, err := settingsManager.GetSettings()
	CheckError(err)
	CheckError(settingsManager.SaveSettings(&settings.ArgoCDSettings{
		// changing theses causes a restart
		AdminPasswordHash:    s.AdminPasswordHash,
		AdminPasswordMtime:   s.AdminPasswordMtime,
		ServerSignature:      s.ServerSignature,
		Certificate:          s.Certificate,
		DexConfig:            s.DexConfig,
		OIDCConfigRAW:        s.OIDCConfigRAW,
		URL:                  s.URL,
		WebhookGitHubSecret:  s.WebhookGitHubSecret,
		WebhookGitLabSecret:  s.WebhookGitLabSecret,
		WebhookBitbucketUUID: s.WebhookBitbucketUUID,
		Secrets:              s.Secrets,
	}))
	SetResourceOverrides(make(map[string]v1alpha1.ResourceOverride))
	SetConfigManagementPlugins()

	// remove tmp dir
	CheckError(os.RemoveAll(tmpDir))

	// name based on test name
	name = dnsFriendly(t.Name())
	// random id - unique across test runs
	id = name + "-" + strings.ToLower(rand.RandString(5))
	repoUrl = fmt.Sprintf("file://%s", repoDirectory())

	// create tmp dir
	FailOnErr(Run("", "mkdir", "-p", tmpDir))

	// set-up tmp repo, must have unique name
	FailOnErr(Run("", "cp", "-Rf", "testdata", repoDirectory()))
	FailOnErr(Run(repoDirectory(), "chmod", "777", "."))
	FailOnErr(Run(repoDirectory(), "git", "init"))
	FailOnErr(Run(repoDirectory(), "git", "add", "."))
	FailOnErr(Run(repoDirectory(), "git", "commit", "-q", "-m", "initial commit"))

	// create namespace
	FailOnErr(Run("", "kubectl", "create", "ns", DeploymentNamespace()))
	FailOnErr(Run("", "kubectl", "label", "ns", DeploymentNamespace(), testingLabel+"=true"))

	log.WithFields(log.Fields{"duration": time.Since(start), "name": name, "id": id}).Info("clean state")
}

func RunCli(args ...string) (string, error) {
	if plainText {
		args = append(args, "--plaintext")
	}

	args = append(args, "--server", apiServerAddress, "--auth-token", token, "--insecure")

	return Run("", "../../dist/argocd", args...)
}

func Patch(path string, jsonPatch string) {

	checkLocalRepo()

	log.WithFields(log.Fields{"path": path, "jsonPatch": jsonPatch}).Info("patching")

	filename := filepath.Join(repoDirectory(), path)
	bytes, err := ioutil.ReadFile(filename)
	CheckError(err)

	patch, err := jsonpatch.DecodePatch([]byte(jsonPatch))
	CheckError(err)

	isYaml := strings.HasSuffix(filename, ".yaml")
	if isYaml {
		log.Info("converting YAML to JSON")
		bytes, err = yaml.YAMLToJSON(bytes)
		CheckError(err)
	}

	log.WithFields(log.Fields{"bytes": string(bytes)}).Info("JSON")

	bytes, err = patch.Apply(bytes)
	CheckError(err)

	if isYaml {
		log.Info("converting JSON back to YAML")
		bytes, err = yaml.JSONToYAML(bytes)
		CheckError(err)
	}

	CheckError(ioutil.WriteFile(filename, bytes, 0644))
	FailOnErr(Run(repoDirectory(), "git", "diff"))
	FailOnErr(Run(repoDirectory(), "git", "commit", "-am", "patch"))
}

func Delete(path string) {

	checkLocalRepo()

	log.WithFields(log.Fields{"path": path}).Info("deleting")

	CheckError(os.Remove(filepath.Join(repoDirectory(), path)))

	FailOnErr(Run(repoDirectory(), "git", "diff"))
	FailOnErr(Run(repoDirectory(), "git", "commit", "-am", "delete"))
}

func checkLocalRepo() {
	if !strings.HasPrefix(repoUrl, "file://") {
		log.WithFields(log.Fields{"repoUrl": repoUrl}).Fatal("cannot patch repo unless it is local")
	}
}

// create the resource by creating using "kubctl apply", with bonus templating
func Declarative(filename string, values interface{}) (string, error) {

	bytes, err := ioutil.ReadFile("testdata/" + filename)
	CheckError(err)

	tmpFile, err := ioutil.TempFile("", "")
	CheckError(err)
	_, err = tmpFile.WriteString(Tmpl(string(bytes), values))
	CheckError(err)

	return Run("", "kubectl", "-n", ArgoCDNamespace, "apply", "-f", tmpFile.Name())
}
