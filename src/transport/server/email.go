package server

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ogen-app/ogen/src/infra/email/resend"
	"github.com/ogen-app/ogen/src/infra/email/templates"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/secrets"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// emailRuntime bundles the CON-154 email collaborators built at boot: the job
// dependency set (injected into the River workers) and the two public handlers.
type emailRuntime struct {
	Deps    queues.EmailDeps
	Handler *handlers.EmailHandler
	Webhook *handlers.ResendWebhookHandler
}

// initEmail wires the transactional + marketing email subsystem (CON-154). The
// Resend sender resolves its key per call from the secrets store, so a key
// set/rotated via the secrets API takes effect with no reboot (an unset key
// makes send jobs log skipped_disabled). Default templates are seeded on boot
// (upsert-if-absent) so a fresh DB has working copy while operator edits are
// preserved.
func initEmail(
	ctx context.Context,
	cfg *config.Config,
	secretStore secrets.Store,
	templateRepo repository.EmailTemplateRepository,
	suppressionRepo repository.EmailSuppressionRepository,
	logRepo repository.EmailLogRepository,
	eventRepo repository.EmailEventRepository,
	bodyRepo repository.EmailBodyRepository,
	userRepo repository.UserRepository,
	activityRecorder *activity.Recorder,
) (emailRuntime, error) {
	apiKey := func(ctx context.Context) (string, error) { return secretStore.Get(ctx, secrets.NameResendAPIKey) }
	linkSecret := func(ctx context.Context) (string, error) { return secretStore.Get(ctx, secrets.NameEmailLinkSecret) }
	webhookSecret := func(ctx context.Context) (string, error) {
		return secretStore.Get(ctx, secrets.NameResendWebhookSecret)
	}

	sender := resend.New(apiKey, cfg.EmailBaseURL, cfg.EmailHTTPTimeout)

	if n, err := templates.SeedDefaults(ctx, templateRepo); err != nil {
		return emailRuntime{}, fmt.Errorf("seed email templates: %w", err)
	} else if n > 0 {
		slog.InfoContext(ctx, "seeded default email templates", logging.AttrComponent, "email", "count", n)
	}

	deps := queues.EmailDeps{
		Sender:       sender,
		Templates:    templateRepo,
		Suppressions: suppressionRepo,
		Logs:         logRepo,
		Bodies:       bodyRepo,
		Users:        userRepo,
		From:         cfg.EmailFrom,
		ReplyTo:      cfg.EmailReplyTo,
		AppBaseURL:   cfg.AppBaseURL,
		LinkBaseURL:  cmp.Or(cfg.EmailLinkBaseURL, cfg.AppBaseURL),
		LinkSecret:   linkSecret,
		Retention:    time.Duration(cfg.EmailLogRetentionDays) * 24 * time.Hour,
		// CON-125: mirror every terminal send outcome into tenant_activity_events.
		ActivityRecorder: activityRecorder,
	}

	return emailRuntime{
		Deps:    deps,
		Handler: handlers.NewEmailHandler(suppressionRepo, linkSecret),
		Webhook: handlers.NewResendWebhookHandler(suppressionRepo, logRepo, eventRepo, webhookSecret),
	}, nil
}
