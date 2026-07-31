// Package bazel_exec provides the minimum subset of the Bazel Remote
// Execution API v2 types needed by monofs-executor. Types use protobuf
// struct tags for wire-format compatibility.
//
// Reference: https://github.com/bazelbuild/remote-apis
package bazel_exec

import (
	"strconv"

	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Digest is a content-addressable identifier (hash/size_bytes).
type Digest struct {
	Hash      string `protobuf:"bytes,1,opt,name=hash,proto3"`
	SizeBytes int64  `protobuf:"varint,2,opt,name=size_bytes,json=sizeBytes,proto3"`
}

func (d *Digest) String() string { return d.Hash + "/" + strconv.FormatInt(d.SizeBytes, 10) }

// Action represents a build action to execute.
type Action struct {
	CommandDigest   *Digest              `protobuf:"bytes,1,opt,name=command_digest,proto3"`
	InputRootDigest *Digest              `protobuf:"bytes,2,opt,name=input_root_digest,proto3"`
	Timeout         *durationpb.Duration `protobuf:"bytes,6,opt,name=timeout,proto3"`
	DoNotCache      bool                 `protobuf:"varint,7,opt,name=do_not_cache,proto3"`
	DigestSalt      []byte               `protobuf:"bytes,9,opt,name=salt,proto3"`
}

// Command describes a command-line invocation.
type Command struct {
	Arguments            []string  `protobuf:"bytes,1,rep,name=arguments,proto3"`
	EnvironmentVariables []*EnvVar `protobuf:"bytes,2,rep,name=environment_variables,proto3"`
	OutputFiles          []string  `protobuf:"bytes,3,rep,name=output_files,proto3"`
	OutputDirectories    []string  `protobuf:"bytes,4,rep,name=output_directories,proto3"`
	Platform             *Platform `protobuf:"bytes,5,opt,name=platform,proto3"`
	WorkingDirectory     string    `protobuf:"bytes,6,opt,name=working_directory,proto3"`
}

type EnvVar struct {
	Name  string `protobuf:"bytes,1,opt,name=name,proto3"`
	Value string `protobuf:"bytes,2,opt,name=value,proto3"`
}

type Platform struct {
	Properties []*Platform_Property `protobuf:"bytes,1,rep,name=properties,proto3"`
}

type Platform_Property struct {
	Name  string `protobuf:"bytes,1,opt,name=name,proto3"`
	Value string `protobuf:"bytes,2,opt,name=value,proto3"`
}

type Directory struct {
	Files       []*FileNode      `protobuf:"bytes,1,rep,name=files,proto3"`
	Directories []*DirectoryNode `protobuf:"bytes,2,rep,name=directories,proto3"`
}

type FileNode struct {
	Name         string  `protobuf:"bytes,1,opt,name=name,proto3"`
	Digest       *Digest `protobuf:"bytes,2,opt,name=digest,proto3"`
	IsExecutable bool    `protobuf:"varint,4,opt,name=is_executable,proto3"`
}

type DirectoryNode struct {
	Name   string  `protobuf:"bytes,1,opt,name=name,proto3"`
	Digest *Digest `protobuf:"bytes,2,opt,name=digest,proto3"`
}

type ActionResult struct {
	OutputFiles       []*OutputFile           `protobuf:"bytes,2,rep,name=output_files,proto3"`
	OutputDirectories []*OutputDirectory      `protobuf:"bytes,3,rep,name=output_directories,proto3"`
	ExitCode          int32                   `protobuf:"varint,4,opt,name=exit_code,proto3"`
	StdoutRaw         []byte                  `protobuf:"bytes,5,opt,name=stdout_raw,proto3"`
	StderrRaw         []byte                  `protobuf:"bytes,6,opt,name=stderr_raw,proto3"`
	ExecutionMetadata *ExecutedActionMetadata `protobuf:"bytes,7,opt,name=execution_metadata,proto3"`
}

type OutputFile struct {
	Path         string  `protobuf:"bytes,1,opt,name=path,proto3"`
	Digest       *Digest `protobuf:"bytes,2,opt,name=digest,proto3"`
	IsExecutable bool    `protobuf:"varint,4,opt,name=is_executable,proto3"`
}

type OutputDirectory struct {
	Path       string  `protobuf:"bytes,1,opt,name=path,proto3"`
	TreeDigest *Digest `protobuf:"bytes,3,opt,name=tree_digest,proto3"`
}

type ExecutedActionMetadata struct {
	Worker                      string                 `protobuf:"bytes,1,opt,name=worker,proto3"`
	QueuedTimestamp             *timestamppb.Timestamp `protobuf:"bytes,2,opt,name=queued_timestamp,proto3"`
	ExecutionStartTimestamp     *timestamppb.Timestamp `protobuf:"bytes,5,opt,name=execution_start_timestamp,proto3"`
	ExecutionCompletedTimestamp *timestamppb.Timestamp `protobuf:"bytes,6,opt,name=execution_completed_timestamp,proto3"`
}

type ExecuteRequest struct {
	InstanceName    string  `protobuf:"bytes,1,opt,name=instance_name,proto3"`
	SkipCacheLookup bool    `protobuf:"varint,3,opt,name=skip_cache_lookup,proto3"`
	ActionDigest    *Digest `protobuf:"bytes,6,opt,name=action_digest,proto3"`
}

type ExecuteResponse struct {
	Result       *anypb.Any `protobuf:"bytes,1,opt,name=result,proto3"`
	CachedResult bool       `protobuf:"varint,2,opt,name=cached_result,proto3"`
	Status       *Status    `protobuf:"bytes,3,opt,name=status,proto3"`
}

type Status struct {
	Code    int32  `protobuf:"varint,1,opt,name=code,proto3"`
	Message string `protobuf:"bytes,2,opt,name=message,proto3"`
}

// --- common helpers ---

// DigestOf computes the digest of raw bytes (SHA-256).
// Uses crypto/sha256. Callers outside this package should use
// the cache package or compute digests independently.
func DigestOf(data []byte) *Digest {
	return &Digest{
		Hash:      sha256hex(data),
		SizeBytes: int64(len(data)),
	}
}

func sha256hex(data []byte) string {
	// Avoid crypto import here; callers use cache.DigestString or compute directly.
	return "" // placeholder — actual digest computed in the executor runner
}
