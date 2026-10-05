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

// AnnotationRequestedBy on an Upgrade names who asked for it: the owner's
// email (the API sets it) or RequestedByAutoUpdate for the update policy.
const AnnotationRequestedBy = "kwerft.dev/requested-by"

// RequestedByAutoUpdate marks Upgrades that AutoPatch created.
const RequestedByAutoUpdate = "auto-update"

// AnnotationCheckUpdatesRequested on ConsoleSettings asks for a release
// discovery now ("Check now"); the value is the time of the request
// (RFC 3339), and a check runs when it is newer than status.updates.checkedAt.
const AnnotationCheckUpdatesRequested = "kwerft.dev/check-updates-requested"

// AnnotationResumeAutoPatch on ConsoleSettings resumes AutoPatch after an
// auto-update failed or rolled back: when its value names the Upgrade in
// status.updates.autoPatchPausedBy, the controller clears the pause.
const AnnotationResumeAutoPatch = "kwerft.dev/resume-autopatch"

// LabelInstaller marks the node the installer ran on (install.sh), where
// /var/lib/kwerft/stages lives; upgrade runners are scheduled there.
const LabelInstaller = "kwerft.dev/installer"
