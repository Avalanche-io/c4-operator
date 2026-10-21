package queue

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Burst spending today survives a restart. A project's daily burst budget
// (Project spec.work.burstBudgetPerDay) is checked against what its units
// spent on burst pools today; kept only in memory, a new leader would start
// the day at nothing and could spend the budget again. The queue writes
// each project's spend, and the day it is for, into the ConfigMap
// spendConfigMap in the operator's namespace at most every spendSaveEvery,
// and the first tick of a leader adds back what it holds for today before
// any burst is decided. A leader change loses at most spendSaveEvery of
// spend; a ConfigMap that cannot be read is tried again each tick, and
// nothing is saved until it has been read, so an outage never writes less
// over more.

const (
	spendConfigMap = "c4-work-burst-spend"
	spendSaveEvery = time.Minute
	// spendDayKey holds the day the amounts are for.
	spendDayKey = "day"
)

// spendLedger is the queue's persisted spend state.
type spendLedger struct {
	loaded bool
	saved  time.Time
	last   map[string]string
}

func spendDay(now time.Time) string { return now.Format("2006-01-02") }

// loadSpend adds back the spend saved for today, once per leader.
func (q *Queue) loadSpend(ctx context.Context, now time.Time) {
	if q.ledger.loaded || q.Namespace == "" || q.Client == nil {
		return
	}
	var cm corev1.ConfigMap
	err := q.Client.Get(ctx, client.ObjectKey{Namespace: q.Namespace, Name: spendConfigMap}, &cm)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		q.Log.Error(err, "burst spend: reading the saved spend; trying again next tick")
		return
	case cm.Data[spendDayKey] == spendDay(now):
		q.spentMu.Lock()
		for p, s := range cm.Data {
			if v, err := strconv.ParseFloat(s, 64); p != spendDayKey && err == nil && v > 0 {
				q.spent[p] += v
			}
		}
		q.spentMu.Unlock()
	}
	q.ledger.loaded = true
	q.ledger.last = cm.Data
}

// saveSpend writes today's spend when it changed, at most every
// spendSaveEvery, and at once on a new day.
func (q *Queue) saveSpend(ctx context.Context, now time.Time) {
	if !q.ledger.loaded || q.Namespace == "" || q.Client == nil {
		return
	}
	day := spendDay(now)
	if q.ledger.last[spendDayKey] == day && now.Sub(q.ledger.saved) < spendSaveEvery {
		return
	}
	data := map[string]string{spendDayKey: day}
	q.spentMu.Lock()
	for p, v := range q.spent {
		data[p] = strconv.FormatFloat(v, 'f', 4, 64)
	}
	q.spentMu.Unlock()
	if equalData(data, q.ledger.last) {
		q.ledger.saved = now
		return
	}
	// A merge patch drops the projects that spent nothing today.
	patch := map[string]any{}
	for k, v := range data {
		patch[k] = v
	}
	for k := range q.ledger.last {
		if _, ok := data[k]; !ok {
			patch[k] = nil
		}
	}
	b, _ := json.Marshal(map[string]any{"data": patch})
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: q.Namespace, Name: spendConfigMap}}
	err := q.Client.Patch(ctx, cm, client.RawPatch(types.MergePatchType, b))
	if apierrors.IsNotFound(err) {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: q.Namespace, Name: spendConfigMap,
			Labels: map[string]string{"app.kubernetes.io/name": "c4-operator", "app.kubernetes.io/component": "queue"}},
			Data: data}
		err = q.Client.Create(ctx, cm)
	}
	if err != nil {
		q.Log.Error(err, "burst spend: saving")
		return
	}
	q.ledger.saved, q.ledger.last = now, data
}

func equalData(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
