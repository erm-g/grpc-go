/*
 *
 * Copyright 2023 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package authz_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/authz"
	"google.golang.org/grpc/authz/audit"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/internal/grpctest"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/testdata"

	_ "google.golang.org/grpc/authz/audit/stdout"
)

func TestAudit(t *testing.T) {
	grpctest.RunSubTests(t, s{})
}

type statAuditLogger struct {
	authzDecisionStat map[bool]int      // Map to hold the counts of authorization decisions
	lastEventContent  map[string]string // Map to hold event fields in key:value fashion
}

func (s *statAuditLogger) Log(event *audit.Event) {
	s.authzDecisionStat[event.Authorized]++
	s.lastEventContent["rpc_method"] = event.FullMethodName
	s.lastEventContent["principal"] = event.Principal
	s.lastEventContent["policy_name"] = event.PolicyName
	s.lastEventContent["matched_rule"] = event.MatchedRule
	s.lastEventContent["authorized"] = strconv.FormatBool(event.Authorized)
}

type loggerBuilder struct {
	authzDecisionStat map[bool]int
	lastEventContent  map[string]string
}

func (loggerBuilder) Name() string {
	return "stat_logger"
}

func (lb *loggerBuilder) Build(audit.LoggerConfig) audit.Logger {
	return &statAuditLogger{
		authzDecisionStat: lb.authzDecisionStat,
		lastEventContent:  lb.lastEventContent,
	}
}

func (*loggerBuilder) ParseLoggerConfig(config json.RawMessage) (audit.LoggerConfig, error) {
	return nil, nil
}

// TestAuditLogger examines audit logging invocations using four different authorization policies.
// It covers scenarios including a disabled audit, auditing both 'allow' and 'deny' outcomes,
// and separately auditing 'allow' and 'deny' outcomes.
// Additionally, it checks if SPIFFE ID from a certificate is propagated correctly.
func (s) TestAuditLogger(t *testing.T) {
	// Each test data entry contains an authz policy for a grpc server,
	// how many 'allow' and 'deny' outcomes we expect (each test case makes 2 unary calls and one client-streaming call),
	// and a structure to check if the audit.Event fields are properly populated.
	tests := []struct {
		name              string
		authzPolicy       string
		wantAuthzOutcomes map[bool]int
		eventContent      map[string]string
	}{
		{
			name: "No audit",
			authzPolicy: `{
				"name": "authz",
				"allow_rules": [
					{
						"name": "allow_UnaryCall",
						"request":
						{
							"paths": [
								"/grpc.testing.TestService/UnaryCall"
							]
						}
					}
				],
				"audit_logging_options": {
					"audit_condition": "NONE",
					"audit_loggers": [
						{
							"name": "stat_logger",
							"config": {},
							"is_optional": false
						}
					]
				}
			}`,
			wantAuthzOutcomes: map[bool]int{true: 0, false: 0},
		},
		{
			name: "Allow All Deny Streaming - Audit All",
			authzPolicy: `{
				"name": "authz",
				"allow_rules": [
					{
						"name": "allow_all",
						"request": {
							"paths": [
								"*"
							]
						}
					}
				],
				"deny_rules": [
					{
						"name": "deny_all",
						"request": {
							"paths":
							[
								"/grpc.testing.TestService/StreamingInputCall"
							]
						}
					}
				],
				"audit_logging_options": {
					"audit_condition": "ON_DENY_AND_ALLOW",
					"audit_loggers": [
						{
							"name": "stat_logger",
							"config": {},
							"is_optional": false
						},
						{
							"name": "stdout_logger",
							"is_optional": false
						}
					]
				}
			}`,
			wantAuthzOutcomes: map[bool]int{true: 2, false: 1},
			eventContent: map[string]string{
				"rpc_method":   "/grpc.testing.TestService/StreamingInputCall",
				"principal":    "spiffe://foo.bar.com/client/workload/1",
				"policy_name":  "authz",
				"matched_rule": "authz_deny_all",
				"authorized":   "false",
			},
		},
		{
			name: "Allow Unary - Audit Allow",
			authzPolicy: `{
				"name": "authz",
				"allow_rules": [
					{
						"name": "allow_UnaryCall",
						"request":
						{
							"paths": [
								"/grpc.testing.TestService/UnaryCall"
							]
						}
					}
				],
				"audit_logging_options": {
					"audit_condition": "ON_ALLOW",
					"audit_loggers": [
						{
							"name": "stat_logger",
							"config": {},
							"is_optional": false
						}
					]
				}
			}`,
			wantAuthzOutcomes: map[bool]int{true: 2, false: 0},
		},
		{
			name: "Allow Typo - Audit Deny",
			authzPolicy: `{
				"name": "authz",
				"allow_rules": [
					{
						"name": "allow_UnaryCall",
						"request":
						{
							"paths": [
								"/grpc.testing.TestService/UnaryCall_Z"
							]
						}
					}
				],
				"audit_logging_options": {
					"audit_condition": "ON_DENY",
					"audit_loggers": [
						{
							"name": "stat_logger",
							"config": {},
							"is_optional": false
						}
					]
				}
			}`,
			wantAuthzOutcomes: map[bool]int{true: 0, false: 3},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Setup test statAuditLogger, gRPC test server with authzPolicy, unary and stream interceptors.
			lb := &loggerBuilder{
				authzDecisionStat: map[bool]int{true: 0, false: 0},
				lastEventContent:  make(map[string]string),
			}
			audit.RegisterLoggerBuilder(lb)
			i, _ := authz.NewStatic(test.authzPolicy)

			s := grpc.NewServer(
				grpc.Creds(loadServerCreds(t)),
				grpc.ChainUnaryInterceptor(i.UnaryInterceptor),
				grpc.ChainStreamInterceptor(i.StreamInterceptor))
			defer s.Stop()
			testgrpc.RegisterTestServiceServer(s, &testServer{})
			lis, err := net.Listen("tcp", "localhost:0")
			if err != nil {
				t.Fatalf("error listening: %v", err)
			}
			go s.Serve(lis)

			// Setup gRPC test client with certificates containing a SPIFFE Id.
			clientConn, err := grpc.Dial(lis.Addr().String(), grpc.WithTransportCredentials(loadClientCreds(t)))
			if err != nil {
				t.Fatalf("grpc.Dial(%v) failed: %v", lis.Addr().String(), err)
			}
			defer clientConn.Close()
			client := testgrpc.NewTestServiceClient(clientConn)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			// Make 2 unary calls and 1 streaming call.
			client.UnaryCall(ctx, &testpb.SimpleRequest{})
			client.UnaryCall(ctx, &testpb.SimpleRequest{})
			stream, err := client.StreamingInputCall(ctx)
			if err != nil {
				t.Fatalf("failed StreamingInputCall err: %v", err)
			}
			req := &testpb.StreamingInputCallRequest{
				Payload: &testpb.Payload{
					Body: []byte("hi"),
				},
			}
			if err := stream.Send(req); err != nil && err != io.EOF {
				t.Fatalf("failed stream.Send err: %v", err)
			}
			stream.CloseAndRecv()

			// Compare expected number of allows/denies with content of internal map of statAuditLogger.
			if diff := cmp.Diff(lb.authzDecisionStat, test.wantAuthzOutcomes); diff != "" {
				t.Fatalf("Authorization decisions do not match\ndiff (-got +want):\n%s", diff)
			}
			// Compare event fields with expected values from authz policy.
			if test.eventContent != nil {
				if diff := cmp.Diff(lb.lastEventContent, test.eventContent); diff != "" {
					t.Fatalf("Unexpected message\ndiff (-got +want):\n%s", diff)
				}
			}
		})
	}
}

func loadServerCreds(t *testing.T) credentials.TransportCredentials {
	cert, err := tls.LoadX509KeyPair(testdata.Path("x509/server1_cert.pem"), testdata.Path("x509/server1_key.pem"))
	if err != nil {
		t.Fatalf("tls.LoadX509KeyPair(x509/server1_cert.pem, x509/server1_key.pem) failed: %v", err)
	}
	ca, err := os.ReadFile(testdata.Path("x509/client_ca_cert.pem"))
	if err != nil {
		t.Fatalf("os.ReadFile(x509/client_ca_cert.pem) failed: %v", err)
	}
	certPool := x509.NewCertPool()
	if !certPool.AppendCertsFromPEM(ca) {
		t.Fatal("failed to append certificates")
	}
	return credentials.NewTLS(&tls.Config{
		ClientAuth:   tls.RequireAndVerifyClientCert,
		Certificates: []tls.Certificate{cert},
		ClientCAs:    certPool,
	})
}

func loadClientCreds(t *testing.T) credentials.TransportCredentials {
	cert, err := tls.LoadX509KeyPair(testdata.Path("x509/client_with_spiffe_cert.pem"), testdata.Path("x509/client_with_spiffe_key.pem"))
	if err != nil {
		t.Fatalf("tls.LoadX509KeyPair(x509/client1_cert.pem, x509/client1_key.pem) failed: %v", err)
	}
	ca, err := os.ReadFile(testdata.Path("x509/server_ca_cert.pem"))
	if err != nil {
		t.Fatalf("os.ReadFile(x509/server_ca_cert.pem) failed: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("failed to append certificates")
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      roots,
		ServerName:   "x.test.example.com",
	})

}
