package controlplane

import (
	"encoding/base64"
	"net"
	"net/url"
	"time"

	envoy_config_cluster_v3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	envoy_config_core_v3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	envoy_config_endpoint_v3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	envoy_extensions_transport_sockets_tls_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	envoy_type_matcher_v3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"github.com/golang/protobuf/ptypes"

	"github.com/pomerium/pomerium/config"
	"github.com/pomerium/pomerium/internal/log"
	"github.com/pomerium/pomerium/internal/urlutil"
)

func (srv *Server) buildClusters(options *config.Options) []*envoy_config_cluster_v3.Cluster {
	grpcURL := &url.URL{
		Scheme: "http",
		Host:   srv.GRPCListener.Addr().String(),
	}
	httpURL := &url.URL{
		Scheme: "http",
		Host:   srv.HTTPListener.Addr().String(),
	}
	authzURL := &url.URL{
		Scheme: options.AuthorizeURL.Scheme,
		Host:   options.AuthorizeURL.Host,
	}

	clusters := []*envoy_config_cluster_v3.Cluster{
		srv.buildInternalCluster("pomerium-control-plane-grpc", grpcURL, true),
		srv.buildInternalCluster("pomerium-control-plane-http", httpURL, false),
		srv.buildInternalCluster("pomerium-authz", authzURL, true),
	}

	if config.IsProxy(options.Services) {
		for _, policy := range options.Policies {
			clusters = append(clusters, srv.buildPolicyCluster(&policy))
		}
	}

	return clusters
}

func (srv *Server) buildInternalCluster(name string, endpoint *url.URL, forceHTTP2 bool) *envoy_config_cluster_v3.Cluster {
	var transportSocket *envoy_config_core_v3.TransportSocket
	if endpoint.Scheme == "https" {
		transportSocket = &envoy_config_core_v3.TransportSocket{
			Name: "tls",
		}
	}
	return srv.buildCluster(name, endpoint, transportSocket, forceHTTP2)
}

func (srv *Server) buildPolicyCluster(policy *config.Policy) *envoy_config_cluster_v3.Cluster {
	name := getPolicyName(policy)
	return srv.buildCluster(name, policy.Destination, srv.buildPolicyTransportSocket(policy), false)
}

func (srv *Server) buildPolicyTransportSocket(policy *config.Policy) *envoy_config_core_v3.TransportSocket {
	if policy.Destination.Scheme != "https" {
		return nil
	}

	sni := policy.Destination.Hostname()
	if policy.TLSServerName != "" {
		sni = policy.TLSServerName
	}
	tlsContext := &envoy_extensions_transport_sockets_tls_v3.UpstreamTlsContext{
		CommonTlsContext: &envoy_extensions_transport_sockets_tls_v3.CommonTlsContext{
			AlpnProtocols: []string{"http/1.1"},
			ValidationContextType: &envoy_extensions_transport_sockets_tls_v3.CommonTlsContext_ValidationContext{
				ValidationContext: srv.buildPolicyValidationContext(policy),
			},
		},
		Sni: sni,
	}
	if policy.ClientCertificate != nil {
		tlsContext.CommonTlsContext.TlsCertificates = append(tlsContext.CommonTlsContext.TlsCertificates,
			envoyTLSCertificateFromGoTLSCertificate(policy.ClientCertificate))
	}

	tlsConfig, _ := ptypes.MarshalAny(tlsContext)
	return &envoy_config_core_v3.TransportSocket{
		Name: "tls",
		ConfigType: &envoy_config_core_v3.TransportSocket_TypedConfig{
			TypedConfig: tlsConfig,
		},
	}
}

func (srv *Server) buildPolicyValidationContext(policy *config.Policy) *envoy_extensions_transport_sockets_tls_v3.CertificateValidationContext {
	sni := policy.Destination.Hostname()
	if policy.TLSServerName != "" {
		sni = policy.TLSServerName
	}
	validationContext := &envoy_extensions_transport_sockets_tls_v3.CertificateValidationContext{
		MatchSubjectAltNames: []*envoy_type_matcher_v3.StringMatcher{{
			MatchPattern: &envoy_type_matcher_v3.StringMatcher_Exact{
				Exact: sni,
			},
		}},
	}
	if policy.TLSCustomCAFile != "" {
		validationContext.TrustedCa = inlineFilename(policy.TLSCustomCAFile)
	} else if policy.TLSCustomCA != "" {
		bs, err := base64.StdEncoding.DecodeString(policy.TLSCustomCA)
		if err != nil {
			log.Error().Err(err).Msg("invalid custom CA certificate")
		}
		validationContext.TrustedCa = inlineBytesAsFilename("custom-ca.pem", bs)
	} else {
		rootCA, err := getRootCertificateAuthority()
		if err != nil {
			log.Error().Err(err).Msg("unable to enable certificate verification because no root CAs were found")
		} else {
			validationContext.TrustedCa = inlineFilename(rootCA)
		}
	}

	if policy.TLSSkipVerify {
		validationContext.TrustChainVerification = envoy_extensions_transport_sockets_tls_v3.CertificateValidationContext_ACCEPT_UNTRUSTED
	}

	return validationContext
}

func (srv *Server) buildCluster(
	name string,
	endpoint *url.URL,
	transportSocket *envoy_config_core_v3.TransportSocket,
	forceHTTP2 bool,
) *envoy_config_cluster_v3.Cluster {
	defaultPort := 80
	if transportSocket != nil && transportSocket.Name == "tls" {
		defaultPort = 443
	}

	cluster := &envoy_config_cluster_v3.Cluster{
		Name:           name,
		ConnectTimeout: ptypes.DurationProto(time.Second * 10),
		LoadAssignment: &envoy_config_endpoint_v3.ClusterLoadAssignment{
			ClusterName: name,
			Endpoints: []*envoy_config_endpoint_v3.LocalityLbEndpoints{{
				LbEndpoints: []*envoy_config_endpoint_v3.LbEndpoint{{
					HostIdentifier: &envoy_config_endpoint_v3.LbEndpoint_Endpoint{
						Endpoint: &envoy_config_endpoint_v3.Endpoint{
							Address: buildAddress(endpoint.Host, defaultPort),
						},
					},
				}},
			}},
		},
		RespectDnsTtl:   true,
		TransportSocket: transportSocket,
	}

	if forceHTTP2 {
		cluster.Http2ProtocolOptions = &envoy_config_core_v3.Http2ProtocolOptions{
			AllowConnect: true,
		}
	}

	if net.ParseIP(urlutil.StripPort(endpoint.Host)) == nil {
		cluster.ClusterDiscoveryType = &envoy_config_cluster_v3.Cluster_Type{Type: envoy_config_cluster_v3.Cluster_LOGICAL_DNS}
	} else {
		cluster.ClusterDiscoveryType = &envoy_config_cluster_v3.Cluster_Type{Type: envoy_config_cluster_v3.Cluster_STATIC}
	}

	return cluster
}