package v1alpha1

// AnnotationRestartedAt on an App asks for a rolling restart: the App
// reconciler copies it onto the pod template, so changing the value (the
// console writes the current time) replaces every replica without a new
// revision.
const AnnotationRestartedAt = "kwerft.dev/restarted-at"

// AnnotationCancelRequested on a Task asks Kwerft to stop the run: the Task
// reconciler deletes its Job (and so its pods) and marks the Task Failed with
// reason Cancelled. The Task itself, its status and its logs' pod name stay.
// The value names who asked (the console writes the user's email).
const AnnotationCancelRequested = "kwerft.dev/cancel-requested"

// AnnotationStartedBy on a Task names the console user who started it ("run
// now"); Tasks a Schedule starts on time carry none.
const AnnotationStartedBy = "kwerft.dev/started-by"
