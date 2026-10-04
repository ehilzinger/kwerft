package v1alpha1

// AnnotationRestartedAt on an App asks for a rolling restart: the App
// reconciler copies it onto the pod template, so changing the value (the
// console writes the current time) replaces every replica without a new
// revision.
const AnnotationRestartedAt = "kwerft.dev/restarted-at"
