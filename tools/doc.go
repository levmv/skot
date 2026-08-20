// Package tools provides Skot's concrete reusable workspace, process, and
// filesystem-boundary implementations. Constructors return ordinary agent.Tool
// values; callers remain responsible for closing resource-owning objects such
// as ProcessManager.
//
// An executable embedding ProcessManager must call
// RunBoundaryChildIfRequested and RunJobWorkerIfRequested before ordinary
// argument parsing. If neither handles the process, it must call
// HardenSupervisor before starting application work.
package tools
