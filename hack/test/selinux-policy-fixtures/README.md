# Effective runtime policy proof

Run `make check-selinux-policy-generated`. It compiles the complete CIL policy
with the repository's tools image and checks the generated binary and contexts.
The same check is a dependency of `make lint`.

`common/runtime-boundaries.cil` contains the production neverallow assertions.
They are evaluated after CIL macro and type-attribute expansion; grepping for an
absent direct allow is not the security proof.

Each fixture contributes exactly one additional CIL statement:

- `widen-*` adds a forbidden permission or attribute membership. Compilation
  must fail against the production neverallow assertions.
- `require-*` adds a neverallow for one required positive permission.
  Compilation must fail because the unmodified policy grants that permission.

The unmodified policy must compile first. Every fixture must then fail with a
neverallow conflict; a syntax error or missing tool/type is a test failure.

These checks protect Cilium runtime state, perf events and pinned BPF objects,
host-observer limits, Kata entrypoints, extension-service execution, MCS
membership, and the sandboxd machine-management boundary. They deliberately
preserve upstream self-BPF operations and generic pod execute/execute_no_trans,
plus the 1.14 tmpfs, overlay, and sandbox service-launch contracts. They do not
claim that SELinux removes every host-device access or that a privileged CRI
workload is isolated from the host.

Compiler proof does not replace the enforcing VM boot, runtime, and AVC gates
against the exact composed installer.
