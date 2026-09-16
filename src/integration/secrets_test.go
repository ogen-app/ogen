//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	secretsv1 "github.com/ogen-app/ogen/gen/secrets/v1"
	"github.com/ogen-app/ogen/src/infra/crypto/envelope"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/secrets"
	grpcserver "github.com/ogen-app/ogen/src/transport/grpc/server"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// Secrets are managed exclusively over the internal gRPC surface (there is no
// REST CRUD). These specs drive the real service — a token-authed gRPC server
// on a random KEK, over a loopback listener — which is the exact path Harbor
// uses, so the KEK crypto, allowlist, redaction, and hot-reload hooks are all
// exercised through the transport.

const secretsGRPCToken = "integration-grpc-token"

// newSecretsStore builds a SecretStore backed by a fresh DB and a random KEK.
func newSecretsStore(db *bun.DB) secrets.Store {
	kek := make([]byte, envelope.KeySize)
	_, err := rand.Read(kek)
	Expect(err).NotTo(HaveOccurred())
	cipher, err := envelope.NewCipher(kek)
	Expect(err).NotTo(HaveOccurred())
	return secrets.NewStore(repository.NewSecretRepository(db), cipher)
}

type secretsGRPCRig struct {
	client secretsv1.SecretsServiceClient
	store  secrets.Store
	addr   string
}

// newSecretsGRPCRig stands up the real gRPC secrets service on a loopback
// listener backed by a real KEK-encrypted store, plus an authenticated client.
func newSecretsGRPCRig() *secretsGRPCRig {
	db := mustOpenIntegrationDB()
	store := newSecretsStore(db)

	srv, err := grpcserver.New(
		secretsGRPCToken, store,
		repository.NewTenantTierRepository(db),
		repository.NewTenantGroupRepository(db),
		repository.NewTenantRepository(db),
		repository.NewPlatformRepository(db),
		repository.NewPlatformGlobalLimitsRepository(db),
		repository.NewTenantTierVersionRepository(db),
		repository.NewTenantTierAssignmentRepository(db),
		repository.NewEmailLogRepository(db),
		repository.NewEmailEventRepository(db),
		nil,
	)
	Expect(err).NotTo(HaveOccurred())
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	go func() { _ = srv.Serve(lis) }()
	DeferCleanup(srv.Stop)

	return &secretsGRPCRig{
		client: dialSecrets(lis.Addr().String(), secretsGRPCToken),
		store:  store,
		addr:   lis.Addr().String(),
	}
}

// dialSecrets returns a SecretsService client that sends `Bearer <token>` on
// every call. An empty token sends no authorization metadata.
func dialSecrets(addr, token string) secretsv1.SecretsServiceClient {
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	if token != "" {
		opts = append(opts, grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, o ...grpc.CallOption) error {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
			return invoker(ctx, method, req, reply, cc, o...)
		}))
	}
	conn, err := grpc.NewClient(addr, opts...)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { _ = conn.Close() })
	return secretsv1.NewSecretsServiceClient(conn)
}

func mustMarshal(m proto.Message) string {
	b, err := proto.Marshal(m)
	Expect(err).NotTo(HaveOccurred())
	return string(b)
}

func setNames(resp *secretsv1.ListResponse) []string {
	var out []string
	for _, s := range resp.GetSecrets() {
		if s.GetSet() {
			out = append(out, s.GetName())
		}
	}
	return out
}

var _ = Describe("Secrets gRPC service lifecycle", func() {
	It("supports the full Set/List/Delete flow without leaking plaintext", func() {
		rig := newSecretsGRPCRig()
		ctx := context.Background()
		const plaintext = "sk-ant-api03-LIFECYCLE-VALUE"

		// List: every allowlist slot present, all unset.
		lresp, err := rig.client.List(ctx, &secretsv1.ListRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(lresp.GetSecrets()).To(HaveLen(len(secrets.AllowedNames)))
		for _, s := range lresp.GetSecrets() {
			Expect(s.GetSet()).To(BeFalse())
		}

		// Set (create) → created=true, decryptable, metadata only.
		sresp, err := rig.client.Set(ctx, &secretsv1.SetRequest{Name: secrets.NameAnthropicAPIKey, Value: plaintext})
		Expect(err).NotTo(HaveOccurred())
		Expect(sresp.GetCreated()).To(BeTrue())
		Expect(sresp.GetSecret().GetName()).To(Equal(secrets.NameAnthropicAPIKey))
		Expect(sresp.GetSecret().GetSet()).To(BeTrue())
		Expect(sresp.GetSecret().GetDecryptable()).To(BeTrue())
		firstUpdate := sresp.GetSecret().GetUpdatedAt().AsTime()

		// The wire responses carry no plaintext.
		Expect(mustMarshal(lresp)).NotTo(ContainSubstring(plaintext))
		Expect(mustMarshal(sresp)).NotTo(ContainSubstring(plaintext))

		// List now shows it set.
		lresp, err = rig.client.List(ctx, &secretsv1.ListRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(setNames(lresp)).To(ContainElement(secrets.NameAnthropicAPIKey))

		// Rotate → created=false, updated_at advances.
		time.Sleep(10 * time.Millisecond) // ensure clock tick
		sresp2, err := rig.client.Set(ctx, &secretsv1.SetRequest{Name: secrets.NameAnthropicAPIKey, Value: plaintext + "-RENEWED"})
		Expect(err).NotTo(HaveOccurred())
		Expect(sresp2.GetCreated()).To(BeFalse())
		Expect(sresp2.GetSecret().GetUpdatedAt().AsTime().After(firstUpdate)).To(BeTrue(),
			"updated_at should advance: was %s, now %s", firstUpdate, sresp2.GetSecret().GetUpdatedAt().AsTime())

		// Store holds the rotated plaintext (decrypts correctly).
		current, err := rig.store.Get(tenantCtx(), secrets.NameAnthropicAPIKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(current).To(Equal(plaintext + "-RENEWED"))

		// Delete → ok; store no longer has it.
		_, err = rig.client.Delete(ctx, &secretsv1.DeleteRequest{Name: secrets.NameAnthropicAPIKey})
		Expect(err).NotTo(HaveOccurred())
		_, err = rig.store.Get(tenantCtx(), secrets.NameAnthropicAPIKey)
		Expect(err).To(MatchError(secrets.ErrNotFound))

		// Delete again → NotFound.
		_, err = rig.client.Delete(ctx, &secretsv1.DeleteRequest{Name: secrets.NameAnthropicAPIKey})
		Expect(status.Code(err)).To(Equal(codes.NotFound))
	})

	It("rejects unknown names with InvalidArgument and never echoes the value", func() {
		rig := newSecretsGRPCRig()
		const sentinel = "SENTINEL_VALUE_SHOULD_NEVER_LEAK"
		_, err := rig.client.Set(context.Background(), &secretsv1.SetRequest{Name: "openai_api_key", Value: sentinel})
		Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
		Expect(err.Error()).NotTo(ContainSubstring(sentinel))
	})

	It("rejects invalid values with InvalidArgument and never echoes the value", func() {
		rig := newSecretsGRPCRig()
		ctx := context.Background()
		const sentinel = "SENTINEL_NEWLINE_ATTEMPT"

		_, err := rig.client.Set(ctx, &secretsv1.SetRequest{Name: secrets.NameAnthropicAPIKey, Value: sentinel + "\n"})
		Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
		Expect(err.Error()).NotTo(ContainSubstring(sentinel))

		_, err = rig.client.Set(ctx, &secretsv1.SetRequest{Name: secrets.NameAnthropicAPIKey, Value: ""})
		Expect(status.Code(err)).To(Equal(codes.InvalidArgument))

		_, err = rig.client.Set(ctx, &secretsv1.SetRequest{Name: secrets.NameAnthropicAPIKey, Value: strings.Repeat("a", secrets.MaxValueLen+1)})
		Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
	})

	It("requires a valid bearer token", func() {
		rig := newSecretsGRPCRig()

		// Same server, but a client that sends no token.
		noauth := dialSecrets(rig.addr, "")
		_, err := noauth.List(context.Background(), &secretsv1.ListRequest{})
		Expect(status.Code(err)).To(Equal(codes.Unauthenticated))

		wrong := dialSecrets(rig.addr, "not-the-token")
		_, err = wrong.List(context.Background(), &secretsv1.ListRequest{})
		Expect(status.Code(err)).To(Equal(codes.Unauthenticated))
	})
})

var _ = Describe("Secrets env-var migration on boot", func() {
	It("migrates env values into the DB on first boot and DB wins thereafter", func() {
		ctx := tenantCtx()
		db := mustOpenIntegrationDB()
		store := newSecretsStore(db)

		// First boot: env vars set, DB empty → migrated.
		result, err := secrets.MigrateFromEnv(ctx, store, []secrets.EnvSource{
			{Name: secrets.NameAnthropicAPIKey, EnvValue: "env-anthropic"},
			{Name: secrets.NameZernioAPIKey, EnvValue: "env-zernio"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.MigratedFromEnv).To(Equal(2))
		Expect(result.IgnoredEnvVars).To(Equal(0))
		Expect(result.SecretsInDB).To(Equal(2))

		got, err := store.Get(ctx, secrets.NameAnthropicAPIKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal("env-anthropic"))

		// Second boot: env vars still set, DB also populated. DB wins,
		// envs ignored and counted.
		result, err = secrets.MigrateFromEnv(ctx, store, []secrets.EnvSource{
			{Name: secrets.NameAnthropicAPIKey, EnvValue: "env-anthropic-NEW-IGNORED"},
			{Name: secrets.NameZernioAPIKey, EnvValue: "env-zernio-NEW-IGNORED"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.MigratedFromEnv).To(Equal(0))
		Expect(result.IgnoredEnvVars).To(Equal(2))
		Expect(result.SecretsInDB).To(Equal(2))

		// DB value is still the original — env did not overwrite it.
		got, err = store.Get(ctx, secrets.NameAnthropicAPIKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal("env-anthropic"))
	})

	It("leaves the app in a degraded state when neither DB nor env has a key", func() {
		ctx := tenantCtx()
		db := mustOpenIntegrationDB()
		store := newSecretsStore(db)

		result, err := secrets.MigrateFromEnv(ctx, store, []secrets.EnvSource{
			{Name: secrets.NameAnthropicAPIKey, EnvValue: ""},
			{Name: secrets.NameZernioAPIKey, EnvValue: ""},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.MigratedFromEnv).To(Equal(0))
		Expect(result.SecretsInDB).To(Equal(0))

		_, err = store.Get(ctx, secrets.NameAnthropicAPIKey)
		Expect(err).To(MatchError(secrets.ErrNotFound))
	})
})

var _ = Describe("Secrets hot-reload subscriber", func() {
	It("notifies subscribers after Set and Delete", func() {
		rig := newSecretsGRPCRig()
		ctx := context.Background()
		var fires int
		rig.store.Subscribe(secrets.NameZernioAPIKey, func() { fires++ })

		_, err := rig.client.Set(ctx, &secretsv1.SetRequest{Name: secrets.NameZernioAPIKey, Value: "key-1"})
		Expect(err).NotTo(HaveOccurred())
		Expect(fires).To(Equal(1))

		_, err = rig.client.Set(ctx, &secretsv1.SetRequest{Name: secrets.NameZernioAPIKey, Value: "key-2"})
		Expect(err).NotTo(HaveOccurred())
		Expect(fires).To(Equal(2))

		_, err = rig.client.Delete(ctx, &secretsv1.DeleteRequest{Name: secrets.NameZernioAPIKey})
		Expect(err).NotTo(HaveOccurred())
		Expect(fires).To(Equal(3))
	})

	// Read-after-write: a Zernio-style resolver wired to the store sees the
	// rotated value on its very next call, no restart.
	It("resolves the new value through a per-call resolver after Set", func() {
		rig := newSecretsGRPCRig()
		ctx := context.Background()
		resolver := func(c context.Context) (string, error) {
			return rig.store.Get(c, secrets.NameZernioAPIKey)
		}

		_, err := rig.client.Set(ctx, &secretsv1.SetRequest{Name: secrets.NameZernioAPIKey, Value: "v1"})
		Expect(err).NotTo(HaveOccurred())
		got, err := resolver(tenantCtx())
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal("v1"))

		_, err = rig.client.Set(ctx, &secretsv1.SetRequest{Name: secrets.NameZernioAPIKey, Value: "v2"})
		Expect(err).NotTo(HaveOccurred())
		got, err = resolver(tenantCtx())
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal("v2"))
	})
})

var _ = Describe("Health endpoint reports secret resolvability", func() {
	It("reports presence + decryptable per allowlisted name", func() {
		db := mustOpenIntegrationDB()
		store := newSecretsStore(db)

		app := fiber.New()
		handlers.NewHealthHandler(db, store).Register(app)

		get := func() map[string]map[string]bool {
			req := httptest.NewRequest("GET", "/api/health", nil)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(200))
			body, _ := io.ReadAll(resp.Body)
			var health struct {
				Status  string                     `json:"status"`
				Secrets map[string]map[string]bool `json:"secrets"`
			}
			Expect(json.Unmarshal(body, &health)).To(Succeed())
			Expect(health.Status).To(Equal("ok"))
			return health.Secrets
		}

		// Boot: no secrets present.
		s := get()
		Expect(s["anthropic_api_key"]["present"]).To(BeFalse())
		Expect(s["zernio_api_key"]["present"]).To(BeFalse())

		// Set a key straight through the store (the gRPC service just wraps this).
		_, _, err := store.Set(tenantCtx(), secrets.NameAnthropicAPIKey, "k")
		Expect(err).NotTo(HaveOccurred())

		s = get()
		Expect(s["anthropic_api_key"]["present"]).To(BeTrue())
		Expect(s["anthropic_api_key"]["decryptable"]).To(BeTrue())
	})
})
