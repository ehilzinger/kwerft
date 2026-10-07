// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package logs

// Field names of a log record in VictoriaLogs. Vector writes them (the VRL
// program vector_remap in install/install.sh; install/test/install.bats
// checks that both sides agree).
const (
	FieldNamespace = "namespace" // the pod's namespace
	FieldPod       = "pod"
	FieldContainer = "container"
	FieldStream    = "stream" // stdout | stderr
	FieldNode      = "node"

	// Pod labels kwerft.dev/<name>. Build pods (in kwerft-builds) carry
	// project too: it names the Build's project, not their namespace, so
	// confinement is always by FieldNamespace.
	FieldProject  = "project"
	FieldApp      = "app"
	FieldBuild    = "build"
	FieldTask     = "task"
	FieldSchedule = "schedule"
	FieldJob      = "job" // the Job owning the pod

	// FieldLevel is level|lvl|severity of a JSON line, lower case.
	FieldLevel = "level"

	// VictoriaLogs' own fields: the time, the raw line and the stream.
	fieldTime     = "_time"
	fieldMsg      = "_msg"
	fieldStreamID = "_stream_id"
)

// streamFields are VictoriaLogs stream fields (VL-Stream-Fields in
// install.sh): constant for a container, so filters on them are stream
// filters, which VictoriaLogs answers without scanning other streams. Only
// these may be used in Scope.Fields.
var streamFields = map[string]bool{
	FieldNamespace: true, FieldPod: true, FieldContainer: true, FieldStream: true,
	FieldProject: true, FieldApp: true, FieldBuild: true, FieldTask: true,
}

// selected are the fields a query returns (the "fields" pipe), so
// VictoriaLogs sends nothing the console does not show.
var selected = []string{
	fieldTime, fieldStreamID, fieldMsg, FieldNamespace, FieldPod, FieldContainer, FieldStream,
	FieldProject, FieldApp, FieldBuild, FieldTask, FieldLevel,
}
