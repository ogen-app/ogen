package queues

import (
	"context"
	"fmt"
)

// NotifyHarborReceiptKind tells Harbor a vendor receipt arrived at the
// receipts inbound address. river-service POSTs the signed event to Harbor's
// inbound webhook; Harbor then fetches and extracts the email with
// receipts.v1 ProcessReceipt.
const NotifyHarborReceiptKind = "notify_harbor_receipt_received"

// NotifyHarborReceiptReceivedTask carries only the Resend email id: no email
// content travels in job arguments.
type NotifyHarborReceiptReceivedTask struct {
	ResendEmailID string `json:"resend_email_id"`
}

// Kind implements JobArgs.
func (NotifyHarborReceiptReceivedTask) Kind() string { return NotifyHarborReceiptKind }

// EnqueueNotifyHarborReceiptReceived queues the receipt notification once per
// Resend email: a redelivered webhook finds the row already written and adds
// nothing. It reports whether a row was written. A nil enqueuer is a no-op.
func (e *Enqueuer) EnqueueNotifyHarborReceiptReceived(ctx context.Context, resendEmailID string) (bool, error) {
	if e == nil || e.outbox == nil {
		return false, nil
	}
	task := NotifyHarborReceiptReceivedTask{ResendEmailID: resendEmailID}
	req, err := enqueueRequest(task, nil)
	if err != nil {
		return false, err
	}
	return e.outbox.WriteOnce(ctx, task.Kind(), fmt.Sprintf("%s:%s", task.Kind(), resendEmailID), req)
}
