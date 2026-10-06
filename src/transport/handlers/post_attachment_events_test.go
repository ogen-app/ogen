package handlers_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// subscribePostEvents subscribes to every entity:post event as userID in
// tenantID, the way the SSE handler does for a session, and unsubscribes when
// the spec ends.
func subscribePostEvents(hub eventhub.Hub, tenantID, userID string) <-chan eventhub.Event {
	GinkgoHelper()
	ch, unsubscribe, err := hub.Subscribe(context.Background(), eventhub.SubscribeOpts{
		UserID:   userID,
		TenantID: tenantID,
		Topics:   []string{"entity:post:*"},
	})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(unsubscribe)
	return ch
}

// nextAttachmentEvent waits for the next post.attachments.changed event.
func nextAttachmentEvent(ch <-chan eventhub.Event) eventhub.Event {
	GinkgoHelper()
	var ev eventhub.Event
	Eventually(ch, 5*time.Second).Should(Receive(&ev))
	Expect(ev.Type).To(Equal(handlers.EventPostAttachmentsChanged))
	return ev
}

// attachmentEventPayload returns the event's hint payload.
func attachmentEventPayload(ev eventhub.Event) map[string]any {
	GinkgoHelper()
	payload, ok := ev.Payload.(map[string]any)
	Expect(ok).To(BeTrue(), "payload is %T", ev.Payload)
	return payload
}

// expectNoPostEvent asserts nothing arrives on ch for a short while.
func expectNoPostEvent(ch <-chan eventhub.Event) {
	GinkgoHelper()
	Consistently(ch, 200*time.Millisecond).ShouldNot(Receive())
}
