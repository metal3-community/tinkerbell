package flag

import (
	"net/netip"
	"time"

	"github.com/peterbourgon/ff/v4/ffval"
	ntip "github.com/tinkerbell/tinkerbell/pkg/flag/netip"
)

type GlobalConfig struct {
	LogLevel             int
	Backend              string
	BackendFilePath      string
	BackendKubeConfig    string
	BackendKubeNamespace string
	OTELEndpoint         string
	OTELInsecure         bool
	TrustedProxies       []netip.Prefix
	PublicIP             netip.Addr
	BindAddr             netip.Addr
	HTTPPort             int
	HTTPSPort            int
	EnableSmee           bool
	EnableTootles        bool
	EnableTinkServer     bool
	EnableTinkController bool
	EnableRufio          bool
	EnableSecondStar     bool
	EnableUI             bool
	EnableConversionWebhook           bool
	ConversionWebhookBindAddr         string
	ConversionWebhookCertFile         string
	ConversionWebhookKeyFile          string
	ConversionWebhookCABundleFile     string
	ConversionWebhookURL              string
	ConversionWebhookServiceName      string
	ConversionWebhookServiceNamespace string
	EnableCRDMigrations  bool
	MaxprocsEnable       bool
	MemlimitRatio        float64
	EmbeddedGlobalConfig EmbeddedGlobalConfig
	BackendKubeOptions   BackendKubeOptions
	TLS                  TLSConfig
}

type EmbeddedGlobalConfig struct {
	EnableKubeAPIServer bool
	EnableETCD          bool
}

type BackendKubeOptions struct {
	QPS                         float32
	Burst                       int
	APIServerHealthTimeout      time.Duration
	APIServerHealthPollInterval time.Duration
}

type TLSConfig struct {
	CertFile                   string
	KeyFile                    string
	DisableHTTPToHTTPSRedirect bool
}

func RegisterGlobal(fs *Set, gc *GlobalConfig) {
	fs.Register(BackendConfig, ffval.NewEnum(&gc.Backend, "kube", "file", "none"))
	fs.Register(BackendFilePath, ffval.NewValueDefault(&gc.BackendFilePath, gc.BackendFilePath))
	fs.Register(KubeBurst, ffval.NewValueDefault(&gc.BackendKubeOptions.Burst, gc.BackendKubeOptions.Burst))
	fs.Register(BackendKubeConfig, ffval.NewValueDefault(&gc.BackendKubeConfig, gc.BackendKubeConfig))
	fs.Register(BackendKubeNamespace, ffval.NewValueDefault(&gc.BackendKubeNamespace, gc.BackendKubeNamespace))
	fs.Register(KubeQPS, ffval.NewValueDefault(&gc.BackendKubeOptions.QPS, gc.BackendKubeOptions.QPS))
	fs.Register(KubeAPIServerHealthTimeout, ffval.NewValueDefault(&gc.BackendKubeOptions.APIServerHealthTimeout, gc.BackendKubeOptions.APIServerHealthTimeout))
	fs.Register(KubeAPIServerHealthPollInterval, ffval.NewValueDefault(&gc.BackendKubeOptions.APIServerHealthPollInterval, gc.BackendKubeOptions.APIServerHealthPollInterval))
	fs.Register(BindAddr, &ntip.Addr{Addr: &gc.BindAddr})
	fs.Register(HTTPPort, ffval.NewValueDefault(&gc.HTTPPort, gc.HTTPPort))
	fs.Register(HTTPSPort, ffval.NewValueDefault(&gc.HTTPSPort, gc.HTTPSPort))
	fs.Register(EnableSmee, ffval.NewValueDefault(&gc.EnableSmee, gc.EnableSmee))
	fs.Register(EnableTootles, ffval.NewValueDefault(&gc.EnableTootles, gc.EnableTootles))
	fs.Register(EnableTinkServer, ffval.NewValueDefault(&gc.EnableTinkServer, gc.EnableTinkServer))
	fs.Register(EnableTinkController, ffval.NewValueDefault(&gc.EnableTinkController, gc.EnableTinkController))
	fs.Register(EnableRufioController, ffval.NewValueDefault(&gc.EnableRufio, gc.EnableRufio))
	fs.Register(EnableSecondStar, ffval.NewValueDefault(&gc.EnableSecondStar, gc.EnableSecondStar))
	fs.Register(EnableUI, ffval.NewValueDefault(&gc.EnableUI, gc.EnableUI))
	fs.Register(EnableConversionWebhook, ffval.NewValueDefault(&gc.EnableConversionWebhook, gc.EnableConversionWebhook))
	fs.Register(ConversionWebhookBindAddr, ffval.NewValueDefault(&gc.ConversionWebhookBindAddr, gc.ConversionWebhookBindAddr))
	fs.Register(ConversionWebhookCertFile, ffval.NewValueDefault(&gc.ConversionWebhookCertFile, gc.ConversionWebhookCertFile))
	fs.Register(ConversionWebhookKeyFile, ffval.NewValueDefault(&gc.ConversionWebhookKeyFile, gc.ConversionWebhookKeyFile))
	fs.Register(ConversionWebhookCABundleFile, ffval.NewValueDefault(&gc.ConversionWebhookCABundleFile, gc.ConversionWebhookCABundleFile))
	fs.Register(ConversionWebhookURL, ffval.NewValueDefault(&gc.ConversionWebhookURL, gc.ConversionWebhookURL))
	fs.Register(ConversionWebhookServiceName, ffval.NewValueDefault(&gc.ConversionWebhookServiceName, gc.ConversionWebhookServiceName))
	fs.Register(ConversionWebhookServiceNamespace, ffval.NewValueDefault(&gc.ConversionWebhookServiceNamespace, gc.ConversionWebhookServiceNamespace))
	fs.Register(EnableCRDMigrations, ffval.NewValueDefault(&gc.EnableCRDMigrations, gc.EnableCRDMigrations))
	fs.Register(LogLevelConfig, ffval.NewValueDefault(&gc.LogLevel, gc.LogLevel))
	fs.Register(OTELEndpoint, ffval.NewValueDefault(&gc.OTELEndpoint, gc.OTELEndpoint))
	fs.Register(OTELInsecure, ffval.NewValueDefault(&gc.OTELInsecure, gc.OTELInsecure))
	fs.Register(PublicIP, &ntip.Addr{Addr: &gc.PublicIP})
	fs.Register(TLSCertFile, ffval.NewValueDefault(&gc.TLS.CertFile, gc.TLS.CertFile))
	fs.Register(TLSKeyFile, ffval.NewValueDefault(&gc.TLS.KeyFile, gc.TLS.KeyFile))
	fs.Register(DisableHTTPToHTTPSRedirect, ffval.NewValueDefault(&gc.TLS.DisableHTTPToHTTPSRedirect, gc.TLS.DisableHTTPToHTTPSRedirect))
	fs.Register(TrustedProxies, &ntip.PrefixList{PrefixList: &gc.TrustedProxies})
	fs.Register(MaxprocsEnable, ffval.NewValueDefault(&gc.MaxprocsEnable, gc.MaxprocsEnable))
	fs.Register(MemlimitRatio, ffval.NewValueDefault(&gc.MemlimitRatio, gc.MemlimitRatio))
}

func RegisterEmbeddedGlobals(fs *Set, gc *GlobalConfig) {
	fs.Register(EnableKubeAPIServer, ffval.NewValueDefault(&gc.EmbeddedGlobalConfig.EnableKubeAPIServer, gc.EmbeddedGlobalConfig.EnableKubeAPIServer))
	fs.Register(EnableETCD, ffval.NewValueDefault(&gc.EmbeddedGlobalConfig.EnableETCD, gc.EmbeddedGlobalConfig.EnableETCD))
}

// All these flags are used by at least two services or
// are used to create objects that are used by multiple services.
var LogLevelConfig = Config{
	Name:  "log-level",
	Usage: "the higher the number the more verbose, a negative number disables logging",
}

// BackendConfig flags.
var BackendConfig = Config{
	Name:  "backend",
	Usage: "backend to use (kube, file, none)",
}

var BackendFilePath = Config{
	Name:  "backend-file-path",
	Usage: "[file] path to the file backend, this is only implemented when running only the Smee service",
}

// Kube backend flags.
var BackendKubeConfig = Config{
	Name:  "backend-kube-config",
	Usage: "[kube] path to the kubeconfig file",
}

var BackendKubeNamespace = Config{
	Name:  "backend-kube-namespace",
	Usage: "[kube] namespace to watch for resources",
}

var KubeQPS = Config{
	Name:  "backend-kube-qps",
	Usage: "[kube] maximum queries per second to the Kubernetes API server. A 0 value equates to 5 (client sdk constraint). A negative value disables client-side ratelimiting.",
}

var KubeBurst = Config{
	Name:  "backend-kube-burst",
	Usage: "[kube] maximum burst for throttle in the Kubernetes client. A 0 value equates to 10 (client sdk constraint). A negative value disables client-side burst limiting.",
}

var KubeAPIServerHealthTimeout = Config{
	Name:  "backend-kube-apiserver-health-timeout",
	Usage: "[kube] maximum time to wait for the API server to become healthy during startup. This prevents permanent error loops on first boot with embedded API server.",
}

var KubeAPIServerHealthPollInterval = Config{
	Name:  "backend-kube-apiserver-health-poll-interval",
	Usage: "[kube] interval between API server health checks during startup.",
}

// OTEL flags.
var OTELEndpoint = Config{
	Name:  "otel-endpoint",
	Usage: "[otel] OpenTelemetry collector endpoint",
}

var OTELInsecure = Config{
	Name:  "otel-insecure",
	Usage: "[otel] OpenTelemetry collector insecure",
}

// Shared flags.
var TrustedProxies = Config{
	Name:  "trusted-proxies",
	Usage: "list of trusted proxies in CIDR notation",
}

var PublicIP = Config{
	Name:  "public-ipv4",
	Usage: "public IPv4 address to use for all enabled services",
}

var EnableSmee = Config{
	Name:  "enable-smee",
	Usage: "enable Smee service",
}

var EnableTootles = Config{
	Name:  "enable-tootles",
	Usage: "enable Tootles service",
}

var EnableTinkServer = Config{
	Name:  "enable-tink-server",
	Usage: "enable Tink Server service",
}

var EnableTinkController = Config{
	Name:  "enable-tink-controller",
	Usage: "enable Tink Controller service",
}

var EnableRufioController = Config{
	Name:  "enable-rufio-controller",
	Usage: "enable Rufio Controller service",
}

var EnableSecondStar = Config{
	Name:  "enable-secondstar",
	Usage: "enable SecondStar service",
}

var EnableUI = Config{
	Name:  "enable-ui",
	Usage: "enable UI service",
}

var EnableKubeAPIServer = Config{
	Name:  "enable-embedded-kube-apiserver",
	Usage: "enables the embedded kube-apiserver",
}

var EnableETCD = Config{
	Name:  "enable-embedded-etcd",
	Usage: "enables the embedded etcd",
}

var EnableCRDMigrations = Config{
	Name:  "enable-crd-migrations",
	Usage: "create CRDs in the cluster",
}

var EnableConversionWebhook = Config{
	Name:  "enable-conversion-webhook",
	Usage: "serve the CRD conversion webhook for Hardware (v1alpha1 ↔ v1alpha2). Requires --conversion-webhook-{cert,key}-file.",
}

var ConversionWebhookBindAddr = Config{
	Name:  "conversion-webhook-bind-addr",
	Usage: "[conversion-webhook] host:port to bind the HTTPS server to",
}

var ConversionWebhookCertFile = Config{
	Name:  "conversion-webhook-cert-file",
	Usage: "[conversion-webhook] path to the TLS certificate (PEM)",
}

var ConversionWebhookKeyFile = Config{
	Name:  "conversion-webhook-key-file",
	Usage: "[conversion-webhook] path to the TLS private key (PEM)",
}

var ConversionWebhookCABundleFile = Config{
	Name:  "conversion-webhook-ca-bundle-file",
	Usage: "[conversion-webhook] path to the PEM-encoded CA bundle the kube apiserver should use to validate the webhook's TLS cert. Required when --enable-conversion-webhook=true to patch conversion=Webhook onto multi-version CRDs.",
}

var ConversionWebhookURL = Config{
	Name:  "conversion-webhook-url",
	Usage: "[conversion-webhook] https:// URL the apiserver should call to reach the webhook (for out-of-cluster setups). Mutually exclusive with --conversion-webhook-service-name.",
}

var ConversionWebhookServiceName = Config{
	Name:  "conversion-webhook-service-name",
	Usage: "[conversion-webhook] name of the in-cluster Service that fronts the webhook. Used with --conversion-webhook-service-namespace.",
}

var ConversionWebhookServiceNamespace = Config{
	Name:  "conversion-webhook-service-namespace",
	Usage: "[conversion-webhook] namespace of the Service named by --conversion-webhook-service-name.",
}

var BindAddr = Config{
	Name:  "bind-address",
	Usage: "IP address to which to bind all services",
}

// TLS flags
var TLSCertFile = Config{
	Name:  "tls-cert-file",
	Usage: "[tls] path to the TLS certificate file",
}

var TLSKeyFile = Config{
	Name:  "tls-key-file",
	Usage: "[tls] path to the TLS key file",
}

var DisableHTTPToHTTPSRedirect = Config{
	Name:  "disable-http-to-https-redirect",
	Usage: "[tls] disable HTTP to HTTPS redirects even when TLS is configured",
}

var HTTPPort = Config{
	Name:  "http-port",
	Usage: "port for the HTTP server",
}

var HTTPSPort = Config{
	Name:  "https-port",
	Usage: "port for the HTTPS server, unused when no TLS cert and key are provided",
}

var MaxprocsEnable = Config{
	Name:  "maxprocs-enable",
	Usage: "automatically set GOMAXPROCS to match Linux container CPU quota via automaxprocs",
}

var MemlimitRatio = Config{
	Name:  "memlimit-ratio",
	Usage: "ratio (0.0-1.0) of cgroup memory limit to use as GOMEMLIMIT via automemlimit (default: 0.9)",
}
