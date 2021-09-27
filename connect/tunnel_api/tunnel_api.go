package tunnel_api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"strconv"
	"strings"
	"text/template"
	"time"

	dhparam "github.com/Luzifer/go-dhparam"
	securityv1 "github.com/openshift/api/security/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	dapi "k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	openvpnClientConfTemplate = `client
dev tun
remote {{ $.Endpoint }} {{ $.Port }} tcp-client
cipher AES-256-CBC
<ca>
{{ .CA }}
</ca>
<key>
{{ .Key }}
</key>
<cert>
{{ .Crt }}
</cert>
verify-x509-name "C=US, ST=NC, L=RDU, O=Engineering, OU=Crane, CN=Server"
`
	openvpnServerConfTemplate = `dh /certs/dh.pem
cipher AES-256-CBC
ca /certs/ca.crt
server 192.168.123.0 255.255.255.0
dev tun0
proto tcp4
port {{ $.Port }}
keepalive 10 120
tmp-dir /tmp/openvpn
cert /certs/server.crt
key /certs/server.key
`
	serviceName   = "openvpn"
	serviceConfig = "openv1pn-conf"
	keySize       = 2048
)

type Tunnel struct {
	DstClient client.Client
	DstConfig *rest.Config
	SrcClient client.Client
	SrcConfig *rest.Config
	Options   Options
}

type Options struct {
	Namespace   string
	CACrt       *bytes.Buffer
	ClientCrt   *bytes.Buffer
	ClientKey   *bytes.Buffer
	ServerCrt   *bytes.Buffer
	ServerKey   *bytes.Buffer
	RSADHKey    *bytes.Buffer
	ClientImage string
	ServerImage string
	ServerPort  int32
}

type openvpnConfigData struct {
	Port     string
	CA       string
	Crt      string
	Key      string
	Endpoint string
}

func Openvpn(tunnel Tunnel) error {
	if tunnel.Options.Namespace == "" {
		tunnel.Options.Namespace = serviceName
	}

	if tunnel.Options.ClientImage == "" {
		tunnel.Options.ClientImage = "quay.io/konveyor/openvpn:latest"
	}

	if tunnel.Options.ServerImage == "" {
		tunnel.Options.ServerImage = "quay.io/konveyor/openvpn:latest"
	}
	if tunnel.Options.ServerPort == 0 {
		tunnel.Options.ServerPort = int32(1194)
	}
	if tunnel.Options.CACrt == nil {
		ca, serverCrt, serverKey, clientCrt, clientKey, dh, err := GenSSLCrts()
		if err != nil {
			return err
		}
		tunnel.Options.CACrt = ca
		tunnel.Options.ServerCrt = serverCrt
		tunnel.Options.ServerKey = serverKey
		tunnel.Options.ClientCrt = clientCrt
		tunnel.Options.ClientKey = clientKey
		tunnel.Options.RSADHKey = dh
	}

	scheme := runtime.NewScheme()
	if err := securityv1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		return err
	}
	dstClient, err := client.New(tunnel.DstConfig, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	srcClient, err := client.New(tunnel.SrcConfig, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	tunnel.DstClient = dstClient
	tunnel.SrcClient = srcClient

	err = createOpenVPNServer(&tunnel)
	if err != nil {
		return err
	}

	err = createOpenVPNClient(&tunnel)
	if err != nil {
		return err
	}

	return err
}

func createOpenVPNServer(tunnel *Tunnel) error {

	deploymentLabels := map[string]string{}
	deploymentLabels["app"] = serviceName

	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: tunnel.Options.Namespace,
		},
	}

	var openvpnConf bytes.Buffer
	openvpnConfTemplate, err := template.New("config").Parse(openvpnServerConfTemplate)
	if err != nil {
		return err
	}

	configdata := openvpnConfigData{
		Port: strconv.Itoa(int(tunnel.Options.ServerPort)),
	}

	err = openvpnConfTemplate.Execute(&openvpnConf, configdata)
	if err != nil {
		return err
	}

	configmap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: *&tunnel.Options.Namespace,
		},
		Data: map[string]string{
			"openvpn.conf": openvpnConf.String(),
		},
	}

	openvpnService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: *&tunnel.Options.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       serviceName,
					Protocol:   corev1.ProtocolTCP,
					Port:       tunnel.Options.ServerPort,
					TargetPort: intstr.IntOrString{Type: intstr.Int, IntVal: tunnel.Options.ServerPort},
				},
			},
			Selector: deploymentLabels,
			Type:     corev1.ServiceTypeLoadBalancer,
		},
	}

	proxiedService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "proxied-cluster",
			Namespace: *&tunnel.Options.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       "proxy",
					Protocol:   corev1.ProtocolTCP,
					Port:       int32(443),
					TargetPort: intstr.IntOrString{Type: intstr.Int, IntVal: 443},
				},
			},
			Selector: deploymentLabels,
			Type:     corev1.ServiceTypeClusterIP,
		},
	}

	serviceAccount := &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: *&tunnel.Options.Namespace,
		},
	}

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: *&tunnel.Options.Namespace,
		},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:           []string{"use"},
				APIGroups:       []string{"security.openshift.io"},
				Resources:       []string{"securitycontextconstraints"},
				ResourceNames:   []string{"privileged"},
				NonResourceURLs: []string{},
			},
		},
	}

	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: *&tunnel.Options.Namespace,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      serviceName,
				Namespace: *&tunnel.Options.Namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     serviceName,
		},
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: *&tunnel.Options.Namespace,
		},
		Data: map[string][]byte{
			"ca.crt":     tunnel.Options.CACrt.Bytes(),
			"dh.pem":     tunnel.Options.RSADHKey.Bytes(),
			"server.crt": tunnel.Options.ServerCrt.Bytes(),
			"server.key": tunnel.Options.ServerKey.Bytes(),
		},
	}

	scc := &securityv1.SecurityContextConstraints{
		ObjectMeta: metav1.ObjectMeta{
			Name: tunnel.Options.Namespace,
		},
		AllowPrivilegedContainer: true,
		AllowedCapabilities:      []corev1.Capability{"*"},
		AllowHostDirVolumePlugin: true,
		Volumes:                  []securityv1.FSType{"*"},
		AllowHostNetwork:         true,
		AllowHostPorts:           true,
		AllowHostPID:             true,
		AllowHostIPC:             true,
		SELinuxContext: securityv1.SELinuxContextStrategyOptions{
			Type: "RunAsAny",
		},
		RunAsUser: securityv1.RunAsUserStrategyOptions{
			Type: "RunAsAny",
		},
		SupplementalGroups: securityv1.SupplementalGroupsStrategyOptions{
			Type: "RunAsAny",
		},
		FSGroup: securityv1.FSGroupStrategyOptions{
			Type: "RunAsAny",
		},
		ReadOnlyRootFilesystem: false,
		Users:                  []string{"system:serviceaccount:" + tunnel.Options.Namespace + ":openvpn"},
		SeccompProfiles:        []string{"*"},
	}

	mode := int32(0400)
	volumes := []v1.Volume{
		{
			Name: serviceName,
			VolumeSource: v1.VolumeSource{
				Secret: &v1.SecretVolumeSource{
					DefaultMode: &mode,
					SecretName:  serviceName,
				},
			},
		},
		{
			Name: serviceConfig,
			VolumeSource: v1.VolumeSource{
				ConfigMap: &v1.ConfigMapVolumeSource{
					LocalObjectReference: v1.LocalObjectReference{
						Name: serviceName,
					},
				},
			},
		},
	}

	volumeMounts := []v1.VolumeMount{
		{
			Name:      serviceName,
			MountPath: "/certs",
		},
		{
			Name:      serviceConfig,
			MountPath: "/openvpn.conf",
			SubPath:   "openvpn.conf",
		},
	}

	trueBool := true
	containers := []corev1.Container{
		{
			Name:  serviceName,
			Image: *&tunnel.Options.ServerImage,
			Command: []string{
				"bash",
				"-c",
				"mkdir /tmp/openvpn && openvpn --config /openvpn.conf",
			},
			VolumeMounts:    volumeMounts,
			SecurityContext: &corev1.SecurityContext{Privileged: &trueBool},
		},
		{
			Name:  "socat",
			Image: *&tunnel.Options.ServerImage,
			Command: []string{
				"bash",
				"-c",
				"socat TCP-LISTEN:443,fork,reuseaddr TCP:192.168.123.6:443",
			},
		},
	}

	rootUser := int64(0)
	podSpec := corev1.PodSpec{
		ServiceAccountName: serviceName,
		SecurityContext:    &corev1.PodSecurityContext{RunAsUser: &rootUser},
		Containers:         containers,
		Volumes:            volumes,
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: *&tunnel.Options.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: deploymentLabels,
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: deploymentLabels,
				},
				Spec: podSpec,
			},
		},
	}

	dapiClient, err := dapi.NewDiscoveryClientForConfig(tunnel.DstConfig)
	if err != nil {
		return err
	}
	version, err := dapiClient.ServerVersion()
	if err != nil {
		return err
	}
	minor, err := strconv.Atoi(strings.Trim(version.Minor, "+"))
	if err != nil {
		return err
	}

	err = tunnel.DstClient.Create(context.TODO(), namespace, &client.CreateOptions{})
	if err != nil {
		return err
	}
	err = tunnel.DstClient.Create(context.TODO(), configmap, &client.CreateOptions{})
	if err != nil {
		return err
	}
	err = tunnel.DstClient.Create(context.TODO(), openvpnService, &client.CreateOptions{})
	if err != nil {
		return err
	}
	err = tunnel.DstClient.Create(context.TODO(), proxiedService, &client.CreateOptions{})
	if err != nil {
		return err
	}
	err = tunnel.DstClient.Create(context.TODO(), serviceAccount, &client.CreateOptions{})
	if err != nil {
		return err
	}
	if minor <= 11 {
		err = tunnel.DstClient.Create(context.TODO(), scc, &client.CreateOptions{})
		if err != nil && !k8serrors.IsAlreadyExists(err) {
			return err
		}
	} else {
		err = tunnel.DstClient.Create(context.TODO(), role, &client.CreateOptions{})
		if err != nil {
			return err
		}
		err = tunnel.DstClient.Create(context.TODO(), roleBinding, &client.CreateOptions{})
		if err != nil {
			return err
		}
	}
	err = tunnel.DstClient.Create(context.TODO(), secret, &client.CreateOptions{})
	if err != nil {
		return err
	}
	err = tunnel.DstClient.Create(context.TODO(), deployment, &client.CreateOptions{})
	if err != nil {
		return err
	}
	return nil
}

func createOpenVPNClient(tunnel *Tunnel) error {
	deploymentLabels := map[string]string{}
	deploymentLabels["app"] = serviceName

	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: tunnel.Options.Namespace,
		},
	}

	var openvpnConf bytes.Buffer
	openvpnConfTemplate, err := template.New("config").Parse(openvpnClientConfTemplate)
	if err != nil {
		return err
	}

	dstService := &corev1.Service{}

	for i := 0; i < 10; i++ {
		err = tunnel.DstClient.Get(context.TODO(), client.ObjectKey{Name: serviceName, Namespace: tunnel.Options.Namespace}, dstService)
		if err != nil {
			return err
		}
		if dstService.Status.LoadBalancer.Ingress != nil {
			break
		}
		time.Sleep(time.Second * 3)
	}

	configdata := openvpnConfigData{
		Port:     strconv.Itoa(int(tunnel.Options.ServerPort)),
		CA:       tunnel.Options.CACrt.String(),
		Key:      tunnel.Options.ClientKey.String(),
		Crt:      tunnel.Options.ClientCrt.String(),
		Endpoint: dstService.Status.LoadBalancer.Ingress[0].Hostname,
	}

	err = openvpnConfTemplate.Execute(&openvpnConf, configdata)
	if err != nil {
		return err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: *&tunnel.Options.Namespace,
		},
		Data: map[string][]byte{
			"openvpn.conf": openvpnConf.Bytes(),
		},
	}

	scc := &securityv1.SecurityContextConstraints{
		ObjectMeta: metav1.ObjectMeta{
			Name: tunnel.Options.Namespace,
		},
		AllowPrivilegedContainer: true,
		AllowedCapabilities:      []corev1.Capability{"*"},
		AllowHostDirVolumePlugin: true,
		Volumes:                  []securityv1.FSType{"*"},
		AllowHostNetwork:         true,
		AllowHostPorts:           true,
		AllowHostPID:             true,
		AllowHostIPC:             true,
		SELinuxContext: securityv1.SELinuxContextStrategyOptions{
			Type: "RunAsAny",
		},
		RunAsUser: securityv1.RunAsUserStrategyOptions{
			Type: "RunAsAny",
		},
		SupplementalGroups: securityv1.SupplementalGroupsStrategyOptions{
			Type: "RunAsAny",
		},
		FSGroup: securityv1.FSGroupStrategyOptions{
			Type: "RunAsAny",
		},
		ReadOnlyRootFilesystem: false,
		Users:                  []string{"system:serviceaccount:" + tunnel.Options.Namespace + ":openvpn"},
		SeccompProfiles:        []string{"*"},
	}

	serviceAccount := &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: *&tunnel.Options.Namespace,
		},
	}

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: *&tunnel.Options.Namespace,
		},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:           []string{"use"},
				APIGroups:       []string{"security.openshift.io"},
				Resources:       []string{"securitycontextconstraints"},
				ResourceNames:   []string{"privileged"},
				NonResourceURLs: []string{},
			},
		},
	}

	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: *&tunnel.Options.Namespace,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      serviceName,
				Namespace: *&tunnel.Options.Namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     serviceName,
		},
	}

	mode := int32(0400)
	volumes := []v1.Volume{
		{
			Name: serviceConfig,
			VolumeSource: v1.VolumeSource{
				Secret: &v1.SecretVolumeSource{
					DefaultMode: &mode,
					SecretName:  serviceName,
				},
			},
		},
	}

	volumeMounts := []v1.VolumeMount{
		{
			Name:      serviceConfig,
			MountPath: "/openvpn.conf",
			SubPath:   "openvpn.conf",
		},
	}

	trueBool := true
	containers := []corev1.Container{
		{
			Name:  serviceName,
			Image: *&tunnel.Options.ServerImage,
			Command: []string{
				"bash",
				"-c",
				"mkdir /tmp/openvpn && openvpn --config /openvpn.conf",
			},
			VolumeMounts:    volumeMounts,
			SecurityContext: &corev1.SecurityContext{Privileged: &trueBool},
		},
		{
			Name:  "socat",
			Image: *&tunnel.Options.ServerImage,
			Command: []string{
				"bash",
				"-c",
				"socat TCP-LISTEN:443,fork,reuseaddr TCP:${KUBERNETES_SERVICE_HOST}:${KUBERNETES_SERVICE_PORT_HTTPS}",
			},
		},
	}

	rootUser := int64(0)
	podSpec := corev1.PodSpec{
		ServiceAccountName: serviceName,
		SecurityContext:    &corev1.PodSecurityContext{RunAsUser: &rootUser},
		Containers:         containers,
		Volumes:            volumes,
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: *&tunnel.Options.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: deploymentLabels,
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: deploymentLabels,
				},
				Spec: podSpec,
			},
		},
	}

	dapiClient, err := dapi.NewDiscoveryClientForConfig(tunnel.SrcConfig)
	if err != nil {
		return err
	}
	version, err := dapiClient.ServerVersion()
	if err != nil {
		return err
	}
	minor, err := strconv.Atoi(strings.Trim(version.Minor, "+"))
	if err != nil {
		return err
	}

	err = tunnel.SrcClient.Create(context.TODO(), namespace, &client.CreateOptions{})
	if err != nil {
		return err
	}
	err = tunnel.SrcClient.Create(context.TODO(), serviceAccount, &client.CreateOptions{})
	if err != nil {
		return err
	}
	if minor <= 11 {
		err = tunnel.SrcClient.Create(context.TODO(), scc, &client.CreateOptions{})
		if err != nil && !k8serrors.IsAlreadyExists(err) {
			return err
		}
	} else {
		err = tunnel.SrcClient.Create(context.TODO(), role, &client.CreateOptions{})
		if err != nil {
			return err
		}
		err = tunnel.SrcClient.Create(context.TODO(), roleBinding, &client.CreateOptions{})
		if err != nil {
			return err
		}
	}
	err = tunnel.SrcClient.Create(context.TODO(), secret, &client.CreateOptions{})
	if err != nil {
		return err
	}
	err = tunnel.SrcClient.Create(context.TODO(), deployment, &client.CreateOptions{})
	if err != nil {
		return err
	}
	return nil
}

func GenSSLCrts() (*bytes.Buffer, *bytes.Buffer, *bytes.Buffer, *bytes.Buffer, *bytes.Buffer, *bytes.Buffer, error) {
	caPrivKey, err := rsa.GenerateKey(rand.Reader, keySize)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}

	subj := pkix.Name{
		CommonName:         "CA",
		Country:            []string{"US"},
		Province:           []string{"NC"},
		Locality:           []string{"RDU"},
		Organization:       []string{"Engineering"},
		OrganizationalUnit: []string{"Crane"},
	}

	caTemp := x509.Certificate{
		SerialNumber:          big.NewInt(2020),
		Subject:               subj,
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	caBytes, err := x509.CreateCertificate(
		rand.Reader,
		&caTemp,
		&caTemp,
		&caPrivKey.PublicKey,
		caPrivKey,
	)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	ca := new(bytes.Buffer)
	err = pem.Encode(ca, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: caBytes,
	})
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	caKey := new(bytes.Buffer)
	err = pem.Encode(caKey, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(caPrivKey),
	})
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}

	serverSubj := pkix.Name{
		CommonName:         "Server",
		Country:            []string{"US"},
		Province:           []string{"NC"},
		Locality:           []string{"RDU"},
		Organization:       []string{"Engineering"},
		OrganizationalUnit: []string{"Crane"},
	}

	serverCrtTemp := x509.Certificate{
		SerialNumber: big.NewInt(1658),
		Subject:      serverSubj,
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		SubjectKeyId: []byte{1, 2, 3, 4, 6},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}

	serverKeyBytes, err := rsa.GenerateKey(rand.Reader, keySize)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}

	serverCrtBytes, err := x509.CreateCertificate(rand.Reader, &serverCrtTemp, &caTemp, &serverKeyBytes.PublicKey, caPrivKey)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}

	serverCrt := new(bytes.Buffer)
	pem.Encode(serverCrt, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: serverCrtBytes,
	})

	serverKey := new(bytes.Buffer)
	pem.Encode(serverKey, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(serverKeyBytes),
	})

	clientSubj := pkix.Name{
		CommonName:         "Client",
		Country:            []string{"US"},
		Province:           []string{"NC"},
		Locality:           []string{"RDU"},
		Organization:       []string{"Engineering"},
		OrganizationalUnit: []string{"Crane"},
	}

	clientCrtTemp := x509.Certificate{
		SerialNumber: big.NewInt(1659),
		Subject:      clientSubj,
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		SubjectKeyId: []byte{1, 2, 3, 4, 6},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}

	clientKeyBytes, err := rsa.GenerateKey(rand.Reader, keySize)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}

	clientCrtBytes, err := x509.CreateCertificate(rand.Reader, &clientCrtTemp, &caTemp, &clientKeyBytes.PublicKey, caPrivKey)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}

	clientCrt := new(bytes.Buffer)
	pem.Encode(clientCrt, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: clientCrtBytes,
	})

	clientKey := new(bytes.Buffer)
	pem.Encode(clientKey, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(clientKeyBytes),
	})

	var cb dhparam.GeneratorCallback

	dhCrtTemp, err := dhparam.Generate(keySize, 2, cb)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}

	dhBytes, err := dhCrtTemp.ToPEM()
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}

	dhCrt := bytes.NewBuffer(dhBytes)

	return ca, serverCrt, serverKey, clientCrt, clientKey, dhCrt, nil
}
