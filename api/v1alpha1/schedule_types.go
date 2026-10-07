// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Concurrency says what a Schedule does when a run is due while an earlier
// one is still going.
const (
	// ConcurrencyForbid waits: the due run starts once the earlier one has
	// finished, if that is still within the starting deadline; else it is
	// skipped.
	ConcurrencyForbid = "Forbid"
	// ConcurrencyReplace stops the earlier run and starts the new one.
	ConcurrencyReplace = "Replace"
	// ConcurrencyAllow starts the new run next to the earlier one.
	ConcurrencyAllow = "Allow"
)

// ScheduleSpec runs a Task on a cron schedule.
type ScheduleSpec struct {
	// Schedule in cron format: "minute hour day-of-month month day-of-week"
	// (e.g. "30 3 * * *"), or @hourly, @daily, @weekly, @monthly, @yearly,
	// @every 2h.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Schedule string `json:"schedule"`

	// TimeZone is an IANA name such as Europe/Berlin. Empty means the time
	// zone of the Kwerft server process, which is UTC in its container.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	TimeZone string `json:"timeZone,omitempty"`

	// Suspend stops new runs; running ones continue.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// Concurrency when a run is due while an earlier one still runs:
	// Forbid (wait, see startingDeadline), Replace or Allow.
	// +kubebuilder:validation:Enum=Forbid;Replace;Allow
	// +kubebuilder:default=Forbid
	// +optional
	Concurrency string `json:"concurrency,omitempty"`

	// StartingDeadline is how late a run may still start: after Kwerft was
	// down, while the schedule was suspended, or while Forbid waited for an
	// earlier run. Of several missed runs only the latest starts. Default 1h.
	// +kubebuilder:default="1h"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('10s')",message="startingDeadline must be at least 10s"
	// +optional
	StartingDeadline *metav1.Duration `json:"startingDeadline,omitempty"`

	// History is how many finished Tasks to keep.
	// +kubebuilder:default={}
	// +optional
	History ScheduleHistory `json:"history,omitempty"`

	// Task is the template for every run.
	Task TaskSpec `json:"task"`
}

// ScheduleHistory limits how many finished runs a Schedule keeps.
type ScheduleHistory struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=3
	// +optional
	Succeeded *int32 `json:"succeeded,omitempty"`

	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=3
	// +optional
	Failed *int32 `json:"failed,omitempty"`
}

// ScheduleStatus is written by the Schedule reconciler.
type ScheduleStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastScheduleTime is the scheduled time of the most recent run started.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`

	// NextScheduleTime is when the next run is due; empty while suspended.
	// +optional
	NextScheduleTime *metav1.Time `json:"nextScheduleTime,omitempty"`

	// LastSuccessTime is when a run last succeeded.
	// +optional
	LastSuccessTime *metav1.Time `json:"lastSuccessTime,omitempty"`

	// LastFailureTime is when a run last failed.
	// +optional
	LastFailureTime *metav1.Time `json:"lastFailureTime,omitempty"`

	// Active lists the Tasks of this Schedule that have not finished.
	// +optional
	Active []string `json:"active,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Schedule creates a Task on a cron schedule.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sched
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 52",message="name must be at most 52 characters (run names append a timestamp)"
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Suspend",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Last",type=date,JSONPath=`.status.lastScheduleTime`
// +kubebuilder:printcolumn:name="Next",type=string,JSONPath=`.status.nextScheduleTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Schedule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ScheduleSpec   `json:"spec"`
	Status ScheduleStatus `json:"status,omitempty"`
}

// ScheduleList contains a list of Schedules.
//
// +kubebuilder:object:root=true
type ScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Schedule `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Schedule{}, &ScheduleList{})
}
