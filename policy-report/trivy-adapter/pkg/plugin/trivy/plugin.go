package trivy

import(
	"encoding/json"
	"fmt"
	"io"
	"time"
	"strings"

	policyreport "sigs.k8s.io/wg-policy-prototypes/policy-report/api/v1alpha2"
	"github.com/aquasecurity/starboard/pkg/kube"
	"github.com/aquasecurity/starboard/pkg/docker"
	"github.com/kubernetes-sigs/wg-policy-prototypes/policy-report/trivy-adapter/pkg/vulnerabilityreport"
	"github.com/kubernetes-sigs/wg-policy-prototypes/policy-report/trivy-adapter/pkg/imgvuln"
	"github.com/aquasecurity/starboard/pkg/ext"
	"github.com/google/go-containerregistry/pkg/name"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
)

func main(){
	fmt.Println("image vulnerability")
}

var (
	defaultResourceRequirements = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("100M"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("500M"),
		},
	}
)

const (
	// Plugin the name of this plugin.
	Plugin = "Trivy"
)

type plugin struct {
	clock       ext.Clock
	idGenerator ext.IDGenerator
}

const (
	keyTrivyImageRef               = "trivy.imageRef"
	keyTrivyMode                   = "trivy.mode"
	keyTrivySeverity               = "trivy.severity"
	keyTrivyIgnoreUnfixed          = "trivy.ignoreUnfixed"
	keyTrivyIgnoreFile             = "trivy.ignoreFile"
	keyTrivyInsecureRegistryPrefix = "trivy.insecureRegistry."
	keyTrivyHTTPProxy              = "trivy.httpProxy"
	keyTrivyHTTPSProxy             = "trivy.httpsProxy"
	keyTrivyNoProxy                = "trivy.noProxy"
	keyTrivyGitHubToken            = "trivy.githubToken"
	keyTrivySkipFiles              = "trivy.skipFiles"
	keyTrivySkipDirs               = "trivy.skipDirs"

	keyTrivyServerURL           = "trivy.serverURL"
	keyTrivyServerTokenHeader   = "trivy.serverTokenHeader"
	keyTrivyServerToken         = "trivy.serverToken"
	keyTrivyServerCustomHeaders = "trivy.serverCustomHeaders"
)

// Mode describes mode in which Trivy client operates.
type Mode string

const (
	Standalone   Mode = "Standalone"
	ClientServer Mode = "ClientServer"
)

// Config defines configuration params for the Trivy vulnerabilityreport.Plugin.
type Config struct {
	imgvuln.PluginConfig
}

func (c Config) GetImageRef() (string, error) {
	return c.GetRequiredData(keyTrivyImageRef)
}

func (c Config) GetMode() (Mode, error) {
	var ok bool
	var value string
	if value, ok = c.Data[keyTrivyMode]; !ok {
		return "", fmt.Errorf("property %s not set", keyTrivyMode)
	}

	switch Mode(value) {
	case Standalone:
		return Standalone, nil
	case ClientServer:
		return ClientServer, nil
	}

	return "", fmt.Errorf("invalid value (%s) of %s; allowed values (%s, %s)",
		value, keyTrivyMode, Standalone, ClientServer)
}

func (c Config) GetServerURL() (string, error) {
	return c.GetRequiredData(keyTrivyServerURL)
}

func (c Config) IgnoreFileExists() bool {
	_, ok := c.Data[keyTrivyIgnoreFile]
	return ok
}

func (c Config) GetInsecureRegistries() map[string]bool {
	insecureRegistries := make(map[string]bool)
	for key, val := range c.Data {
		if strings.HasPrefix(key, keyTrivyInsecureRegistryPrefix) {
			insecureRegistries[val] = true
		}
	}

	return insecureRegistries
}

// NewPlugin constructs a new vulnerabilityreport.Plugin, which is using an
// upstream Trivy container image to scan Kubernetes workloads.
//
// This Plugin supports both Standalone and ClientServer modes depending on
// the settings returned by Config.GetMode.
//
// The ClientServer mode is usually more performant, however it
// requires a Trivy server accessible at the configurable Config.GetServerURL.
func NewPlugin(clock ext.Clock, idGenerator ext.IDGenerator) vulnerabilityreport.Plugin {
	return &plugin{
		clock:       clock,
		idGenerator: idGenerator,
	}
}

// Init ensures the default Config required by this plugin.
func (p *plugin) Init(ctx imgvuln.PluginContext) error {
	return ctx.EnsureConfig(imgvuln.PluginConfig{
		Data: map[string]string{
			keyTrivyImageRef: "docker.io/aquasec/trivy:0.16.0",
			keyTrivySeverity: "UNKNOWN,LOW,MEDIUM,HIGH,CRITICAL",
			keyTrivyMode:     string(Standalone),
		},
	})
}

func (p *plugin) GetScanJobSpec(ctx imgvuln.PluginContext, spec corev1.PodSpec, credentials map[string]docker.Auth) (corev1.PodSpec, []*corev1.Secret, error) {
	config, err := p.newConfigFrom(ctx)
	if err != nil {
		return corev1.PodSpec{}, nil, err
	}

	mode, err := config.GetMode()
	if err != nil {
		return corev1.PodSpec{}, nil, err
	}
	switch mode {
	case Standalone:
		return p.getPodSpecForStandaloneMode(config, spec, credentials)
	case ClientServer:
		return p.getPodSpecForClientServerMode(config, spec, credentials)
	default:
		return corev1.PodSpec{}, nil, fmt.Errorf("unrecognized trivy mode: %v", mode)
	}
}

func (p *plugin) newSecretWithAggregateImagePullCredentials(spec corev1.PodSpec, credentials map[string]docker.Auth) *corev1.Secret {
	containerImages := kube.GetContainerImagesFromPodSpec(spec)
	secretData := kube.AggregateImagePullSecretsData(containerImages, credentials)

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			// TODO Use deterministic names for secrets that hold image pull credentials, e.g. scan-vulnerabilityreport-<workload hash>-registry-creds
			Name: p.idGenerator.GenerateID(),
		},
		Data: secretData,
	}
}

const (
	sharedVolumeName     = "data"
	ignoreFileVolumeName = "ignorefile"
)

// In the Standalone mode there is the init container responsible for
// downloading the latest Trivy DB file from GitHub and storing it to the empty
// volume shared with main containers. In other words, the init container runs
// the following Trivy command:
//
//     trivy --download-db-only --cache-dir /var/lib/trivy
//
// The number of main containers correspond to the number of containers
// defined for the scanned workload. Each container runs the Trivy image scan
// command and skips the database download:
//
//     trivy --skip-update --cache-dir /var/lib/trivy \
//       --format json <container image>
func (p *plugin) getPodSpecForStandaloneMode(config Config, spec corev1.PodSpec, credentials map[string]docker.Auth) (corev1.PodSpec, []*corev1.Secret, error) {
	var secret *corev1.Secret
	var secrets []*corev1.Secret

	if len(credentials) > 0 {
		secret = p.newSecretWithAggregateImagePullCredentials(spec, credentials)
		secrets = append(secrets, secret)
	}

	trivyImageRef, err := config.GetImageRef()
	if err != nil {
		return corev1.PodSpec{}, nil, err
	}

	trivyConfigName := imgvuln.GetPluginConfigMapName(Plugin)

	initContainer := corev1.Container{
		Name:                     p.idGenerator.GenerateID(),
		Image:                    trivyImageRef,
		ImagePullPolicy:          corev1.PullIfNotPresent,
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		Env: []corev1.EnvVar{
			{
				Name: "HTTP_PROXY",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: trivyConfigName,
						},
						Key:      keyTrivyHTTPProxy,
						Optional: pointer.BoolPtr(true),
					},
				},
			},
			{
				Name: "HTTPS_PROXY",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: trivyConfigName,
						},
						Key:      keyTrivyHTTPSProxy,
						Optional: pointer.BoolPtr(true),
					},
				},
			},
			{
				Name: "NO_PROXY",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: trivyConfigName,
						},
						Key:      keyTrivyNoProxy,
						Optional: pointer.BoolPtr(true),
					},
				},
			},
			{
				Name: "GITHUB_TOKEN",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: trivyConfigName,
						},
						Key:      keyTrivyGitHubToken,
						Optional: pointer.BoolPtr(true),
					},
				},
			},
		},
		Command: []string{
			"trivy",
		},
		Args: []string{
			"--download-db-only",
			"--cache-dir",
			"/var/lib/trivy",
		},
		Resources: defaultResourceRequirements,
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      sharedVolumeName,
				MountPath: "/var/lib/trivy",
				ReadOnly:  false,
			},
		},
	}

	var containers []corev1.Container

	volumeMounts := []corev1.VolumeMount{
		{
			Name:      sharedVolumeName,
			ReadOnly:  false,
			MountPath: "/var/lib/trivy",
		},
	}
	volumes := []corev1.Volume{
		{
			Name: sharedVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium: corev1.StorageMediumDefault,
				},
			},
		},
	}

	if config.IgnoreFileExists() {
		volumes = append(volumes, corev1.Volume{
			Name: ignoreFileVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: trivyConfigName,
					},
					Items: []corev1.KeyToPath{
						{
							Key:  keyTrivyIgnoreFile,
							Path: ".trivyignore",
						},
					},
				},
			},
		})

		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      ignoreFileVolumeName,
			MountPath: "/tmp/trivy/.trivyignore",
			SubPath:   ".trivyignore",
		})
	}

	for _, c := range spec.Containers {

		env := []corev1.EnvVar{
			{
				Name: "TRIVY_SEVERITY",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: trivyConfigName,
						},
						Key:      keyTrivySeverity,
						Optional: pointer.BoolPtr(true),
					},
				},
			},
			{
				Name: "TRIVY_IGNORE_UNFIXED",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: trivyConfigName,
						},
						Key:      keyTrivyIgnoreUnfixed,
						Optional: pointer.BoolPtr(true),
					},
				},
			},
			{
				Name: "TRIVY_SKIP_FILES",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: trivyConfigName,
						},
						Key:      keyTrivySkipFiles,
						Optional: pointer.BoolPtr(true),
					},
				},
			},
			{
				Name: "TRIVY_SKIP_DIRS",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: trivyConfigName,
						},
						Key:      keyTrivySkipDirs,
						Optional: pointer.BoolPtr(true),
					},
				},
			},
			{
				Name: "HTTP_PROXY",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: trivyConfigName,
						},
						Key:      keyTrivyHTTPProxy,
						Optional: pointer.BoolPtr(true),
					},
				},
			},
			{
				Name: "HTTPS_PROXY",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: trivyConfigName,
						},
						Key:      keyTrivyHTTPSProxy,
						Optional: pointer.BoolPtr(true),
					},
				},
			},
			{
				Name: "NO_PROXY",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: trivyConfigName,
						},
						Key:      keyTrivyNoProxy,
						Optional: pointer.BoolPtr(true),
					},
				},
			},
		}

		if config.IgnoreFileExists() {
			env = append(env, corev1.EnvVar{
				Name:  "TRIVY_IGNOREFILE",
				Value: "/tmp/trivy/.trivyignore",
			})
		}

		if _, ok := credentials[c.Name]; ok && secret != nil {
			registryUsernameKey := fmt.Sprintf("%s.username", c.Name)
			registryPasswordKey := fmt.Sprintf("%s.password", c.Name)

			env = append(env, corev1.EnvVar{
				Name: "TRIVY_USERNAME",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: secret.Name,
						},
						Key: registryUsernameKey,
					},
				},
			}, corev1.EnvVar{
				Name: "TRIVY_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: secret.Name,
						},
						Key: registryPasswordKey,
					},
				},
			})
		}

		env, err = p.appendTrivyInsecureEnv(config, c.Image, env)
		if err != nil {
			return corev1.PodSpec{}, nil, err
		}

		containers = append(containers, corev1.Container{
			Name:                     c.Name,
			Image:                    trivyImageRef,
			ImagePullPolicy:          corev1.PullIfNotPresent,
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			Env:                      env,
			Command: []string{
				"trivy",
			},
			Args: []string{
				"--skip-update",
				"--cache-dir",
				"/var/lib/trivy",
				"--quiet",
				"--format",
				"json",
				c.Image,
			},
			Resources:    defaultResourceRequirements,
			VolumeMounts: volumeMounts,
			SecurityContext: &corev1.SecurityContext{
				Privileged:               pointer.BoolPtr(false),
				AllowPrivilegeEscalation: pointer.BoolPtr(false),
				Capabilities: &corev1.Capabilities{
					Drop: []corev1.Capability{"all"},
				},
				ReadOnlyRootFilesystem: pointer.BoolPtr(true),
			},
		})
	}

	return corev1.PodSpec{
		Affinity:                     imgvuln.LinuxNodeAffinity(),
		RestartPolicy:                corev1.RestartPolicyNever,
		AutomountServiceAccountToken: pointer.BoolPtr(false),
		Volumes:                      volumes,
		InitContainers:               []corev1.Container{initContainer},
		Containers:                   containers,
		SecurityContext:              &corev1.PodSecurityContext{},
	}, secrets, nil
}

// In the ClientServer mode the number of containers of the pod
// created by the scan job equals the number of containers defined for the
// scanned workload. Each container runs Trivy image scan command and refers
// to Trivy server URL returned by Config.GetServerURL:
//
//     trivy client --remote <server URL> \
//       --format json <container image ref>
func (p *plugin) getPodSpecForClientServerMode(config Config, spec corev1.PodSpec, credentials map[string]docker.Auth) (corev1.PodSpec, []*corev1.Secret, error) {
	var secret *corev1.Secret
	var secrets []*corev1.Secret
	var volumeMounts []corev1.VolumeMount
	var volumes []corev1.Volume

	trivyImageRef, err := config.GetImageRef()
	if err != nil {
		return corev1.PodSpec{}, nil, err
	}

	trivyServerURL, err := config.GetServerURL()
	if err != nil {
		return corev1.PodSpec{}, nil, err
	}

	if len(credentials) > 0 {
		secret = p.newSecretWithAggregateImagePullCredentials(spec, credentials)
		secrets = append(secrets, secret)
	}

	var containers []corev1.Container

	trivyConfigName := imgvuln.GetPluginConfigMapName(Plugin)

	for _, container := range spec.Containers {

		env := []corev1.EnvVar{
			{
				Name: "HTTP_PROXY",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: trivyConfigName,
						},
						Key:      keyTrivyHTTPProxy,
						Optional: pointer.BoolPtr(true),
					},
				},
			},
			{
				Name: "HTTPS_PROXY",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: trivyConfigName,
						},
						Key:      keyTrivyHTTPSProxy,
						Optional: pointer.BoolPtr(true),
					},
				},
			},
			{
				Name: "NO_PROXY",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: trivyConfigName,
						},
						Key:      keyTrivyNoProxy,
						Optional: pointer.BoolPtr(true),
					},
				},
			},
			{
				Name: "TRIVY_SEVERITY",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: trivyConfigName,
						},
						Key:      keyTrivySeverity,
						Optional: pointer.BoolPtr(true),
					},
				},
			},
			{
				Name: "TRIVY_IGNORE_UNFIXED",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: trivyConfigName,
						},
						Key:      keyTrivyIgnoreUnfixed,
						Optional: pointer.BoolPtr(true),
					},
				},
			},
			{
				Name: "TRIVY_SKIP_FILES",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: trivyConfigName,
						},
						Key:      keyTrivySkipFiles,
						Optional: pointer.BoolPtr(true),
					},
				},
			},
			{
				Name: "TRIVY_SKIP_DIRS",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: trivyConfigName,
						},
						Key:      keyTrivySkipDirs,
						Optional: pointer.BoolPtr(true),
					},
				},
			},
			{
				Name: "TRIVY_TOKEN_HEADER",
				ValueFrom: &corev1.EnvVarSource{
					ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: trivyConfigName,
						},
						Key:      keyTrivyServerTokenHeader,
						Optional: pointer.BoolPtr(true),
					},
				},
			},
			{
				Name: "TRIVY_TOKEN",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: trivyConfigName,
						},
						Key:      keyTrivyServerToken,
						Optional: pointer.BoolPtr(true),
					},
				},
			},
			{
				Name: "TRIVY_CUSTOM_HEADERS",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: trivyConfigName,
						},
						Key:      keyTrivyServerCustomHeaders,
						Optional: pointer.BoolPtr(true),
					},
				},
			},
		}

		if _, ok := credentials[container.Name]; ok && secret != nil {
			registryUsernameKey := fmt.Sprintf("%s.username", container.Name)
			registryPasswordKey := fmt.Sprintf("%s.password", container.Name)

			env = append(env, corev1.EnvVar{
				Name: "TRIVY_USERNAME",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: secret.Name,
						},
						Key: registryUsernameKey,
					},
				},
			}, corev1.EnvVar{
				Name: "TRIVY_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: secret.Name,
						},
						Key: registryPasswordKey,
					},
				},
			})
		}

		env, err = p.appendTrivyInsecureEnv(config, container.Image, env)
		if err != nil {
			return corev1.PodSpec{}, nil, err
		}

		if config.IgnoreFileExists() {
			volumes = []corev1.Volume{
				{
					Name: ignoreFileVolumeName,
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: trivyConfigName,
							},
							Items: []corev1.KeyToPath{
								{
									Key:  keyTrivyIgnoreFile,
									Path: ".trivyignore",
								},
							},
						},
					},
				},
			}

			volumeMounts = []corev1.VolumeMount{
				{
					Name:      ignoreFileVolumeName,
					MountPath: "/tmp/trivy/.trivyignore",
					SubPath:   ".trivyignore",
				},
			}

			env = append(env, corev1.EnvVar{
				Name:  "TRIVY_IGNOREFILE",
				Value: "/tmp/trivy/.trivyignore",
			})
		}

		containers = append(containers, corev1.Container{
			Name:                     container.Name,
			Image:                    trivyImageRef,
			ImagePullPolicy:          corev1.PullIfNotPresent,
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			Env:                      env,
			Command: []string{
				"trivy",
			},
			Args: []string{
				"--quiet",
				"client",
				"--format",
				"json",
				"--remote",
				trivyServerURL,
				container.Image,
			},
			VolumeMounts: volumeMounts,
			Resources:    defaultResourceRequirements,
		})
	}

	return corev1.PodSpec{
		RestartPolicy:                corev1.RestartPolicyNever,
		AutomountServiceAccountToken: pointer.BoolPtr(false),
		Containers:                   containers,
		Volumes:                      volumes,
	}, secrets, nil
}

func (p *plugin) appendTrivyInsecureEnv(config Config, image string, env []corev1.EnvVar) ([]corev1.EnvVar, error) {
	ref, err := name.ParseReference(image)
	if err != nil {
		return nil, err
	}

	insecureRegistries := config.GetInsecureRegistries()
	if insecureRegistries[ref.Context().RegistryStr()] {
		env = append(env, corev1.EnvVar{
			Name:  "TRIVY_INSECURE",
			Value: "true",
		})
	}

	return env, nil
}


const PolicyReportSource string = "Trivy"

func (p *plugin) ParsePolicyReportData(ctx imgvuln.PluginContext, imageRef string, logsReader io.ReadCloser) (policyreport.PolicyReport, error) {

	var policyReport policyreport.PolicyReport

	var reports []ScanReport
	err := json.NewDecoder(logsReader).Decode(&reports)
	if err != nil {
		return policyreport.PolicyReport{}, err
	}

	for _, report := range reports {
		for _, sr := range report.Vulnerabilities {
			r := newResult(sr)
			policyReport.Results = append(policyReport.Results, r)
		}
	}

	policyReport.Summary = p.toSummaryPolicy(policyReport.Results)

	return policyReport, nil
}

func newResult(result Vulnerability) *policyreport.PolicyReportResult {
	//r := result.References
	s := GetScoreFromCVSS(result.Cvss)
	b := false
	if s != nil{
		b = true
	}

	var sev string = string(result.Severity)
	var r policyreport.PolicyResultSeverity = policyreport.PolicyResultSeverity(strings.ToLower(sev))

	t := time.Now()
	tUnix := t.Unix()
	tUnixNano := int32(t.UnixNano())

	policyR := &policyreport.PolicyReportResult{
		Timestamp:	 metav1.Timestamp{Nanos: tUnixNano, Seconds: tUnix,},
		Policy:      result.Title,
		Source:      PolicyReportSource,
		Category:    result.PkgName,
		Scored:      b,
		Properties: map[string]string{
			"VulnerabilityID":           result.VulnerabilityID,
			"InstalledVersion":           result.InstalledVersion,
			"FixedVersion":        result.FixedVersion,
			"PrimaryURL":     result.PrimaryURL,
			//"References":            strings.Join(r," "),
		},
	}
	if r == "high" || r == "low" || r == "medium"{
		policyR.Severity = r
	}
	
	return policyR
}


func (p *plugin) newConfigFrom(ctx imgvuln.PluginContext) (Config, error) {
	pluginConfig, err := ctx.GetConfig()
	if err != nil {
		return Config{}, err
	}
	return Config{PluginConfig: pluginConfig}, nil
}


func (p *plugin) toSummaryPolicy(results []*policyreport.PolicyReportResult) policyreport.PolicyReportSummary {
	var rs policyreport.PolicyReportSummary
	for _, v := range results {
		switch v.Severity {
		case "low":
			rs.Error++
		case "high":
			rs.Fail++
		case "medium":
			rs.Warn++
		case "none":
			rs.Pass++
		default:
			rs.Skip++
		}
	}
	return rs
}


func GetScoreFromCVSS(CVSSs map[string]*CVSS) *float64 {
	var nvdScore, vendorScore *float64

	for name, cvss := range CVSSs {
		if name == "nvd" {
			nvdScore = cvss.V3Score
		} else {
			vendorScore = cvss.V3Score
		}
	}

	if vendorScore != nil {
		return vendorScore
	}

	return nvdScore
}