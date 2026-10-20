package lease

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Agent is the holder's agent for one lease: it creates the lease, keeps
// its Secret holding the holder's current short-lived token, renews it
// before it expires, and when the lease ends or Run's context is done,
// deletes the Secret and marks the lease revoked or expired.
type Agent struct {
	Client    client.Client
	Namespace string
	Name      string
	Spec      c4v1.CredentialLeaseSpec
	Source    Source
	// Retry is how often it looks again for a renewed sign-in when the one
	// it has expires within RenewBefore (default a minute).
	Retry time.Duration
	Now   func() time.Time
	Logf  func(format string, args ...any)
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Agent) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
	}
}

// Run keeps the lease until it ends or ctx is done, and ends it.
func (a *Agent) Run(ctx context.Context) error {
	l, err := a.ensure(ctx)
	if err != nil {
		return err
	}
	retry := a.Retry
	if retry <= 0 {
		retry = time.Minute
	}
	start := l.CreationTimestamp.Time
	if start.IsZero() {
		start = a.now()
	}
	end := start.Add(a.Spec.TTL.Duration)
	var next time.Time // when the token is due for renewal
	for {
		// the farm may have ended the lease (its switch off, a lapse): the
		// agent does not issue it again
		if state, why, err := a.farmEnded(ctx, l); err != nil {
			a.logf("lease %s/%s: %v", a.Namespace, a.Name, err)
		} else if state != "" {
			return a.finish(context.WithoutCancel(ctx), l, state, "the farm ended the lease: "+why)
		}
		if !a.now().Before(next) {
			var err error
			if next, err = a.renew(ctx, l, end); err != nil {
				a.logf("lease %s/%s: %v", a.Namespace, a.Name, err)
				next = a.now().Add(retry)
			}
		}
		if !a.now().Before(end) {
			return a.finish(context.WithoutCancel(ctx), l, c4v1.LeaseExpired, "the lease's TTL ended")
		}
		// wake for the renewal, the lease's end, or a look at its state
		wake := a.now().Add(retry)
		if next.Before(wake) {
			wake = next
		}
		if end.Before(wake) {
			wake = end
		}
		select {
		case <-ctx.Done():
			return a.finish(context.WithoutCancel(ctx), l, c4v1.LeaseRevoked, "the holder's agent stopped")
		case <-time.After(time.Until(wake)):
		}
	}
}

// farmEnded reports the state the farm ended the lease in (revoked or
// expired, by the controller), or "".
func (a *Agent) farmEnded(ctx context.Context, l *c4v1.CredentialLease) (string, string, error) {
	var cur c4v1.CredentialLease
	if err := a.Client.Get(ctx, types.NamespacedName{Namespace: l.Namespace, Name: l.Name}, &cur); err != nil {
		return "", "", err
	}
	switch cur.Status.State {
	case c4v1.LeaseRevoked, c4v1.LeaseExpired:
		return cur.Status.State, cur.Status.Message, nil
	}
	return "", "", nil
}

// ensure creates the lease, or brings an existing one's spec in line.
func (a *Agent) ensure(ctx context.Context) (*c4v1.CredentialLease, error) {
	l := &c4v1.CredentialLease{ObjectMeta: metav1.ObjectMeta{Namespace: a.Namespace, Name: a.Name}, Spec: a.Spec}
	err := a.Client.Create(ctx, l)
	if apierrors.IsAlreadyExists(err) {
		if err := a.Client.Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: a.Name}, l); err != nil {
			return nil, err
		}
		l.Spec = a.Spec
		if err = a.Client.Update(ctx, l); err == nil {
			// a new run of the agent issues the lease afresh
			orig := l.DeepCopy()
			l.Status = c4v1.CredentialLeaseStatus{State: c4v1.LeaseRenewing, Message: "the holder's agent started"}
			err = a.Client.Status().Patch(ctx, l, client.MergeFrom(orig))
		}
	}
	if err != nil {
		return nil, fmt.Errorf("lease %s/%s: %w", a.Namespace, a.Name, err)
	}
	return l, nil
}

// renew writes the holder's current token into the Secret and the lease's
// status, and returns when to look again.
func (a *Agent) renew(ctx context.Context, l *c4v1.CredentialLease, end time.Time) (time.Time, error) {
	doc, err := a.Source.Read(ctx)
	if err != nil {
		return time.Time{}, a.status(ctx, l, c4v1.LeaseWaitingForHolder, nil, err.Error())
	}
	cred, expires, err := ShortLived(doc)
	clear(doc)
	if err != nil {
		return time.Time{}, a.status(ctx, l, c4v1.LeaseWaitingForHolder, nil, fmt.Sprintf("%s: %v", a.Source.Where(), err))
	}
	now := a.now()
	if !now.Before(expires) {
		return time.Time{}, a.status(ctx, l, c4v1.LeaseWaitingForHolder, nil,
			"the holder's sign-in expired at "+expires.Format(time.RFC3339)+"; it renews when Claude Code on the holder's machine refreshes it")
	}
	if err := a.writeSecret(ctx, l, cred); err != nil {
		return time.Time{}, err
	}
	clear(cred)
	until := expires
	if end.Before(until) {
		until = end
	}
	t := metav1.NewTime(until)
	if err := a.status(ctx, l, c4v1.LeaseActive, &t, ""); err != nil {
		return time.Time{}, err
	}
	a.logf("lease %s/%s: active until %s (the token from %s expires %s)", a.Namespace, a.Name,
		until.Local().Format("15:04:05"), a.Source.Where(), expires.Local().Format("15:04:05"))
	return expires.Add(-a.Spec.RenewBefore.Duration), nil
}

func (a *Agent) writeSecret(ctx context.Context, l *c4v1.CredentialLease, cred []byte) error {
	own := metav1.OwnerReference{APIVersion: c4v1.GroupVersion.String(), Kind: "CredentialLease", Name: l.Name, UID: l.UID}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: l.Namespace, Name: l.SecretName(),
		Labels: map[string]string{"c4.avalanche.io/lease": l.Name}, OwnerReferences: []metav1.OwnerReference{own}},
		Type: corev1.SecretTypeOpaque, Data: map[string][]byte{c4v1.LeaseCredentialKey: cred}}
	err := a.Client.Create(ctx, sec)
	if apierrors.IsAlreadyExists(err) {
		err = a.Client.Update(ctx, sec)
	}
	if err != nil {
		// the API's error names the Secret, not its data
		return fmt.Errorf("the lease's Secret %s: %v", l.SecretName(), apierrors.ReasonForError(err))
	}
	return nil
}

func (a *Agent) status(ctx context.Context, l *c4v1.CredentialLease, state string, expires *metav1.Time, msg string) error {
	if err := a.Client.Get(ctx, types.NamespacedName{Namespace: l.Namespace, Name: l.Name}, l); err != nil {
		return err
	}
	orig := l.DeepCopy()
	l.Status.State, l.Status.Message = state, msg
	if expires != nil {
		now := metav1.NewTime(a.now())
		l.Status.ExpiresAt, l.Status.RenewedAt = expires, &now
	}
	return a.Client.Status().Patch(ctx, l, client.MergeFrom(orig))
}

// finish ends the lease: its Secret deleted, its state saying why.
func (a *Agent) finish(ctx context.Context, l *c4v1.CredentialLease, state, why string) error {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: l.Namespace, Name: l.SecretName()}}
	if err := a.Client.Delete(ctx, sec); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting the lease's Secret %s: %v", l.SecretName(), apierrors.ReasonForError(err))
	}
	a.logf("lease %s/%s: %s; its Secret %s is deleted", a.Namespace, a.Name, why, l.SecretName())
	return a.status(ctx, l, state, nil, why)
}
