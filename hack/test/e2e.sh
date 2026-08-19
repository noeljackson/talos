#!/usr/bin/env bash

# This file contains common environment variables and setup logic for all test
# scripts. It assumes that the following environment variables are set by the
# Makefile:
#  - PLATFORM
#  - TAG
#  - SHA
#  - REGISTRY
#  - IMAGE
#  - INSTALLER_IMAGE
#  - ARTIFACTS
#  - TALOSCTL
#  - INTEGRATION_TEST
#  - SHORT_INTEGRATION_TEST
#  - CUSTOM_CNI_URL
#  - KUBECTL
#  - KUBESTR
#  - HELM
#  - CILIUM_CLI

set -eoux pipefail

TMP="/tmp/e2e/${PLATFORM}"
mkdir -p "${TMP}"

# Talos

export TALOSCONFIG="${TMP}/talosconfig"
TALOS_VERSION=$(cut -d "." -f 1,2 <<< "${TAG}")
export TALOS_VERSION

# Kubernetes

export KUBECONFIG="${TMP}/kubeconfig"
export KUBERNETES_VERSION=${KUBERNETES_VERSION:-1.36.3}

export NAME_PREFIX="talos-e2e-${SHA}-${PLATFORM}"
export TIMEOUT=1200

# default values, overridden by talosctl cluster create tests
PROVISIONER=
CLUSTER_NAME=

TEST_SHORT=()
TEST_RUN=("-test.run" ".")

function run_talos_integration_test {
  case "${SHORT_INTEGRATION_TEST:-no}" in
    no)
      ;;
    *)
      TEST_SHORT=("-test.short")
      ;;
  esac

  case "${WITH_AIRGAPPED:-false}" in
    no-proxy)
      TEST_AIRGAPPED=("-talos.airgapped")
      ;;
    *)
      ;;
  esac

  case "${INTEGRATION_TEST_RUN:-no}" in
    no)
      ;;
    *)
      TEST_RUN=("-test.run" "${INTEGRATION_TEST_RUN}")
      ;;
  esac

  if [ -n "${QEMU_EXTRA_DISKS_TAGS:-}" ]; then
      TEST_VIRTIOFSD=("-talos.virtiofsd")
  fi

  "${INTEGRATION_TEST}" \
    -test.v \
    -talos.failfast \
    -talos.talosctlpath "${TALOSCTL}" \
    -talos.kubectlpath "${KUBECTL}" \
    -talos.helmpath "${HELM}" \
    -talos.kubestrpath "${KUBESTR}" \
    -talos.provisioner "${PROVISIONER}" \
    -talos.name "${CLUSTER_NAME}" \
    -talos.image "${REGISTRY}/siderolabs/talos" \
    ${EXTRA_TEST_ARGS:-} \
    "${TEST_RUN[@]}" \
    "${TEST_SHORT[@]}" \
    "${TEST_AIRGAPPED[@]}" \
    "${TEST_VIRTIOFSD[@]}"
}

function run_talos_integration_test_docker {
  case "${SHORT_INTEGRATION_TEST:-no}" in
    no)
      ;;
    *)
      TEST_SHORT=("-test.short")
      ;;
  esac

  case "${INTEGRATION_TEST_RUN:-no}" in
    no)
      ;;
    *)
      TEST_RUN=("-test.run" "${INTEGRATION_TEST_RUN}")
      ;;
  esac

  "${INTEGRATION_TEST}" \
    -test.v \
    -talos.failfast \
    -talos.talosctlpath "${TALOSCTL}" \
    -talos.kubectlpath "${KUBECTL}" \
    -talos.helmpath "${HELM}" \
    -talos.kubestrpath "${KUBESTR}" \
    -talos.provisioner "${PROVISIONER}" \
    -talos.name "${CLUSTER_NAME}" \
    -talos.image "${REGISTRY}/siderolabs/talos" \
    ${EXTRA_TEST_ARGS:-} \
    "${TEST_RUN[@]}" \
    "${TEST_SHORT[@]}"
}

function run_kubernetes_conformance_test {
  "${TALOSCTL}" conformance kubernetes --mode="${1}"
}

function run_kubernetes_integration_test {
  "${TALOSCTL}" health --run-e2e
}

function run_control_plane_cis_benchmark {
  ${KUBECTL} apply -f "${PWD}/hack/test/cis/kube-bench-master.yaml"
  ${KUBECTL} wait --timeout=300s --for=condition=complete job/kube-bench-master > /dev/null
  ${KUBECTL} logs job/kube-bench-master
}

function run_worker_cis_benchmark {
  ${KUBECTL} apply -f "${PWD}/hack/test/cis/kube-bench-node.yaml"
  ${KUBECTL} wait --timeout=300s --for=condition=complete job/kube-bench-node > /dev/null
  ${KUBECTL} logs job/kube-bench-node
}

function get_kubeconfig {
  rm -f "${TMP}/kubeconfig"
  "${TALOSCTL}" kubeconfig "${TMP}"
}

function dump_cluster_state {
  nodes=$(${KUBECTL} get nodes -o jsonpath="{.items[*].status.addresses[?(@.type == 'InternalIP')].address}" | tr '[:space:]' ',')
  "${TALOSCTL}" -n "${nodes}" services
  ${KUBECTL} get nodes -o wide
  ${KUBECTL} get pods --all-namespaces -o wide
}

function build_image_cache {
  cat _out/integration-images.txt | "${TALOSCTL}" image cache-create --images=- --image-cache-path="${TMP}/image-cache" --layout=flat

  "${TALOSCTL}" image cache-cert-gen \
    --tls-ca-file="${TMP}/image-cache-ca.crt" \
    --tls-cert-file="${TMP}/image-cache-tls.crt" \
    --tls-key-file="${TMP}/image-cache-tls.key" \
    --advertised-address="172.20.1.1"

  cat image-cache-patch.yaml
  mv image-cache-patch.yaml "${TMP}/image-cache-patch.yaml"
}

function build_registry_mirrors {
  if [[ "${WITH_AIRGAPPED:-false}" == "no-proxy" ]]; then
    build_image_cache

    REGISTRY_MIRROR_FLAGS=()

    for registry in docker.io registry.k8s.io quay.io gcr.io ghcr.io; do
      addr="172.20.1.1"
      REGISTRY_MIRROR_FLAGS+=("--registry-mirror=${registry}=https://${addr}:5000")
    done

    return
  fi

  if [[ "${REGISTRY_MIRROR_FLAGS:-yes}" == "no" ]]; then
    REGISTRY_MIRROR_FLAGS=()

    return
  fi

  if [[ "${CI:-false}" == "true" ]]; then
    REGISTRY_MIRROR_FLAGS=()

    for registry in docker.io registry.k8s.io quay.io gcr.io ghcr.io; do
      local service="registry-${registry//./-}.ci.svc"
      addr=$(python3 -c "import socket; print(socket.gethostbyname('${service}'))")

      REGISTRY_MIRROR_FLAGS+=("--registry-mirror=${registry}=http://${addr}:5000")
    done
  fi
}

function install_and_run_cilium_cni_tests {
  get_kubeconfig

  if [[ "${KATA_RUNTIME_TEST:-false}" == "true" ]]; then
    assert_kata_runtime_candidate >/dev/null
  fi

  CILIUM_SELINUX_ARGS=()

  if [[ "${WITH_CILIUM_SELINUX_LABELS:-${WITH_ENFORCING:-false}}" == "true" ]]; then
    # Talos owns this dedicated domain and precreates its runtime hostPath.
    # OpenShift's default spc_t is not a Talos policy type.
    CILIUM_SELINUX_ARGS=(
      --set=podSecurityContext.seLinuxOptions.type=cilium_t
      --set=podSecurityContext.seLinuxOptions.level=s0
      --set=securityContext.seLinuxOptions.type=cilium_t
      --set=securityContext.seLinuxOptions.level=s0
      --set=envoy.podSecurityContext.seLinuxOptions.type=cilium_t
      --set=envoy.podSecurityContext.seLinuxOptions.level=s0
      --set=envoy.securityContext.seLinuxOptions.type=cilium_t
      --set=envoy.securityContext.seLinuxOptions.level=s0
    )
  fi

  case "${WITH_KUBESPAN:-false}" in
    true)
      CILIUM_NODE_ENCRYPTION=false
      CILIUM_TEST_EXTRA_ARGS=("--test=!node-to-node-encryption,!check-log-errors,!pod-to-pod-encryption-v2")
      ;;
    *)
      CILIUM_NODE_ENCRYPTION=true
      CILIUM_TEST_EXTRA_ARGS=("--test=!check-log-errors")
      ;;
  esac

  case "${CILIUM_INSTALL_TYPE:-none}" in
    strict)
      ${CILIUM_CLI} install \
        --set=ipam.mode=kubernetes \
        --set=kubeProxyReplacement=true \
        --set=encryption.nodeEncryption=${CILIUM_NODE_ENCRYPTION} \
        --set=securityContext.capabilities.ciliumAgent="{CHOWN,KILL,NET_ADMIN,NET_RAW,IPC_LOCK,SYS_ADMIN,SYS_RESOURCE,DAC_OVERRIDE,FOWNER,SETGID,SETUID}" \
        --set=securityContext.capabilities.cleanCiliumState="{NET_ADMIN,SYS_ADMIN,SYS_RESOURCE}" \
        --set=cgroup.autoMount.enabled=false \
        --set=cgroup.hostRoot=/sys/fs/cgroup \
        --set=sysctlfix.enabled=false \
        --set=k8sServiceHost=localhost \
        --set=k8sServicePort=13336 \
        "${CILIUM_SELINUX_ARGS[@]}"
      ;;
    *)
      # explicitly setting kubeProxyReplacement=disabled since by the time cilium cli runs talos
      # has not yet applied the kube-proxy manifests
      ${CILIUM_CLI} install \
        --set=ipam.mode=kubernetes \
        --set=kubeProxyReplacement=false \
        --set=encryption.nodeEncryption=${CILIUM_NODE_ENCRYPTION} \
        --set=securityContext.capabilities.ciliumAgent="{CHOWN,KILL,NET_ADMIN,NET_RAW,IPC_LOCK,SYS_ADMIN,SYS_RESOURCE,DAC_OVERRIDE,FOWNER,SETGID,SETUID}" \
        --set=securityContext.capabilities.cleanCiliumState="{NET_ADMIN,SYS_ADMIN,SYS_RESOURCE}" \
        --set=cgroup.autoMount.enabled=false \
        --set=cgroup.hostRoot=/sys/fs/cgroup \
        --set=sysctlfix.enabled=false \
        "${CILIUM_SELINUX_ARGS[@]}"
      ;;
  esac

  ${CILIUM_CLI} status --wait --wait-duration=10m

  if [[ "${WITH_ENFORCING:-false}" == "true" ]]; then
    # A first start does not exercise Cilium's persistent host paths after a
    # new pod receives projected configuration. Roll both agents once so the
    # enforcing gate catches MCS mismatches on projected ConfigMaps and stale
    # CNI/BPF/runtime objects before an image reaches physical hardware.
    ${KUBECTL} -n kube-system rollout restart daemonset/cilium daemonset/cilium-envoy
    ${KUBECTL} -n kube-system rollout status daemonset/cilium --timeout=10m
    ${KUBECTL} -n kube-system rollout status daemonset/cilium-envoy --timeout=10m
    ${CILIUM_CLI} status --wait --wait-duration=10m

    # DaemonSet readiness does not exercise CRI invoking the installed CNI
    # binaries. Launch one ordinary pod on every node after the restart so the
    # gate covers the complete containerd -> loopback -> cilium-cni sandbox
    # path, including OverlayFS lower-inode permission checks on /opt.
    CILIUM_PROBE_IMAGE=$(${KUBECTL} -n kube-system get daemonset/cilium-envoy \
      -o jsonpath='{.spec.template.spec.containers[0].image}')
    mapfile -t CILIUM_PROBE_NODES < <(${KUBECTL} get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
    CILIUM_PROBE_INDEX=0

    for node_name in "${CILIUM_PROBE_NODES[@]}"; do
      CILIUM_PROBE_INDEX=$((CILIUM_PROBE_INDEX + 1))
      probe_name="cni-exec-probe-${CILIUM_PROBE_INDEX}"
      ${KUBECTL} -n kube-system run "${probe_name}" \
        --image="${CILIUM_PROBE_IMAGE}" \
        --restart=Never \
        --overrides="$(printf '{\"spec\":{\"nodeName\":\"%s\"}}' "${node_name}")" \
        --command -- /bin/sh -c 'sleep 300'
      ${KUBECTL} -n kube-system wait pod/"${probe_name}" \
        --for=condition=Ready --timeout=5m
      ${KUBECTL} -n kube-system delete pod/"${probe_name}" \
        --wait=true --timeout=2m
    done

    # A Ready Cilium pod proves networking, but it does not prove that CRI
    # honored the requested SELinux domain. Verify the running host processes
    # so a disabled CRI SELinux integration cannot pass this gate as pod_t.
    CILIUM_NODE_IPS=$(${KUBECTL} get pods -n kube-system \
      -l 'k8s-app in (cilium,cilium-envoy)' \
      -o jsonpath='{range .items[*]}{.status.hostIP}{"\n"}{end}' | sort -u)

    while IFS= read -r node_ip; do
      [[ -n "${node_ip}" ]] || continue

      ${TALOSCTL} -n "${node_ip}" processes | awk '
        /cilium-agent|cilium-envoy/ {
          found = 1
          if ($0 !~ /system_u:system_r:cilium_t:s0/) {
            print "unexpected Cilium SELinux process label: " $0 > "/dev/stderr"
            bad = 1
          }
        }
        END {
          if (!found) {
            print "no Cilium host processes found" > "/dev/stderr"
            exit 1
          }
          exit bad
        }
      '

      CILIUM_AVC_COUNT=$(${TALOSCTL} -n "${node_ip}" dmesg | awk '
        /type=AVC|avc: *denied/ { count++ }
        END { print count + 0 }
      ')

      if [[ "${CILIUM_AVC_COUNT}" -ne 0 ]]; then
        echo "SELinux reported ${CILIUM_AVC_COUNT} AVC denials on ${node_ip}" >&2

        return 1
      fi
    done <<< "${CILIUM_NODE_IPS}"
  fi

  if [[ "${CILIUM_SKIP_CONNECTIVITY_TEST:-false}" == "true" ]]; then
    return
  fi

  if [[ -n "${CILIUM_CONNECTIVITY_TEST:-}" ]]; then
    CILIUM_CONNECTIVITY_JUNIT="${TMP}/cilium-connectivity.xml"
    rm -f "${CILIUM_CONNECTIVITY_JUNIT}"

    CILIUM_TEST_EXTRA_ARGS+=("--test=${CILIUM_CONNECTIVITY_TEST}")
    CILIUM_TEST_EXTRA_ARGS+=("--junit-file=${CILIUM_CONNECTIVITY_JUNIT}")
  fi

  # ref: https://github.com/cilium/cilium-cli/releases/tag/v0.16.14
  ${KUBECTL} delete ns --ignore-not-found cilium-test-1 cilium-test-ccnp1 cilium-test-ccnp2

  ${KUBECTL} create ns cilium-test-1
  ${KUBECTL} create ns cilium-test-ccnp1
  ${KUBECTL} create ns cilium-test-ccnp2
  ${KUBECTL} label ns cilium-test-1 cilium-test-ccnp1 cilium-test-ccnp2 pod-security.kubernetes.io/enforce=privileged

  # --external-target added, as default 'one.one.one.one' is buggy, and CloudFlare status is of course "all healthy"
  ${CILIUM_CLI} connectivity test --test-namespace cilium-test --external-target google.com --timeout=20m "${CILIUM_TEST_EXTRA_ARGS[@]}"

  if [[ -n "${CILIUM_CONNECTIVITY_TEST:-}" ]]; then
    CILIUM_EXECUTED_TESTS=$(awk '/<testcase / && $0 !~ /status="skipped"/ { count++ } END { print count + 0 }' "${CILIUM_CONNECTIVITY_JUNIT}")

    if [[ "${CILIUM_EXECUTED_TESTS}" -eq 0 ]]; then
      echo "Cilium connectivity selector matched zero runnable tests: ${CILIUM_CONNECTIVITY_TEST}" >&2

      return 1
    fi
  fi

  ${KUBECTL} delete ns cilium-test-1 cilium-test-ccnp1 cilium-test-ccnp2
}

function assert_kata_runtime_candidate {
  local deadline
  local extension_inventory
  local node_ip
  local node_name
  local server_version
  local service_inventory
  local -a worker_nodes

  deadline=$((SECONDS + 300))

  while (( SECONDS < deadline )); do
    mapfile -t worker_nodes < <(${KUBECTL} get nodes \
      -l '!node-role.kubernetes.io/control-plane' \
      -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)

    [[ ${#worker_nodes[@]} -eq 1 ]] && break

    sleep 2
  done

  if [[ ${#worker_nodes[@]} -ne 1 ]]; then
    echo "expected exactly one QEMU worker for the Kata runtime probe, found ${#worker_nodes[@]}" >&2

    return 1
  fi

  node_name=${worker_nodes[0]}
  node_ip=$(${KUBECTL} get node "${node_name}" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')

  if [[ -z ${node_ip} ]]; then
    echo "Kata runtime worker ${node_name} has no InternalIP" >&2

    return 1
  fi

  if [[ -z "${KATA_RUNTIME_EXPECTED_TALOS_VERSION:-}" ]]; then
    echo "KATA_RUNTIME_EXPECTED_TALOS_VERSION must identify the exact candidate" >&2

    return 1
  fi

  server_version=$(${TALOSCTL} -n "${node_ip}" version --json)

  if ! jq -es --arg expected "${KATA_RUNTIME_EXPECTED_TALOS_VERSION}" \
    'length == 1 and .[0].version.tag == $expected' <<< "${server_version}" >/dev/null; then
    echo "Kata runtime worker does not run the exact expected Talos version ${KATA_RUNTIME_EXPECTED_TALOS_VERSION}" >&2

    return 1
  fi

  extension_inventory=$(${TALOSCTL} -n "${node_ip}" get extensions -o json)

  if ! jq -es '
    def named($name): any(.[].spec.metadata.name?;
      . == $name or endswith("/" + $name));
    named("kata-containers")
  ' <<< "${extension_inventory}" >/dev/null; then
    echo "Kata runtime worker is missing the kata-containers system extension" >&2

    return 1
  fi

  deadline=$((SECONDS + 300))

  while (( SECONDS < deadline )); do
    service_inventory=$(${TALOSCTL} -n "${node_ip}" get services.v1alpha1.talos.dev -o json)

    if jq -es '
      def running($id): any(.[];
        .metadata.id == $id and .spec.running == true);
      def healthy($id): any(.[];
        .metadata.id == $id and .spec.running == true and .spec.healthy == true);
      healthy("containerd") and healthy("cri") and healthy("kubelet") and
      running("ext-nydus-for-kata-tee")
    ' <<< "${service_inventory}" >/dev/null; then
      printf '%s %s\n' "${node_name}" "${node_ip}"

      return 0
    fi

    sleep 2
  done

  echo "Kata runtime worker did not satisfy the required service contract" >&2
  ${TALOSCTL} -n "${node_ip}" services >&2 || true

  return 1
}

function run_kata_runtime_test {
  local avc_count
  local deadline
  local node_ip
  local node_name
  local pod_state
  local probe_name="kata-runtime-enforcing-probe"
  local runtime_class_name="kata-runtime-enforcing"
  local runtime_handler
  local terminal_event

  runtime_handler="${KATA_RUNTIME_HANDLER:-}"

  if [[ ! ${runtime_handler} =~ ^kata(-[a-z0-9][a-z0-9-]*)?$ ]]; then
    echo "KATA_RUNTIME_HANDLER must be a concrete Kata handler name" >&2

    return 1
  fi

  read -r node_name node_ip < <(assert_kata_runtime_candidate)

  ${KUBECTL} apply -f - <<EOF
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: ${runtime_class_name}
handler: ${runtime_handler}
---
apiVersion: v1
kind: Pod
metadata:
  name: ${probe_name}
spec:
  nodeName: ${node_name}
  restartPolicy: Never
  runtimeClassName: ${runtime_class_name}
  containers:
    - name: probe
      image: registry.k8s.io/pause:3.10
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop:
            - ALL
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
EOF

  function kata_runtime_avc_count {
    ${TALOSCTL} -n "${node_ip}" logs auditd | awk '
      /type=AVC|avc: *denied/ { count++ }
      END { print count + 0 }
    '
  }

  function dump_kata_runtime_failure {
    # These are structural QEMU test-cluster diagnostics. Emit the complete
    # service, kernel, and audit buffers so a new failure cannot be hidden
    # behind a stale classifier or a secondary record ceiling. Talos routes
    # enforcing AVC records to auditd, so dmesg alone is not an SELinux
    # oracle.
    avc_count=$(kata_runtime_avc_count)
    echo "Kata runtime audit evidence on ${node_name}: avc_denials=${avc_count}" >&2
    ${KUBECTL} describe pod "${probe_name}" || true
    ${TALOSCTL} -n "${node_ip}" services || true
    ${TALOSCTL} -n "${node_ip}" logs containerd || true
    ${TALOSCTL} -n "${node_ip}" logs auditd || true
    ${TALOSCTL} -n "${node_ip}" dmesg || true
  }

  deadline=$((SECONDS + 300))

  while (( SECONDS < deadline )); do
    pod_state=$(${KUBECTL} get pod "${probe_name}" -o json)

    if jq -e 'any(.status.conditions[]?; .type == "Ready" and .status == "True")' \
      <<< "${pod_state}" >/dev/null; then
      break
    fi

    terminal_event=$(${KUBECTL} get events \
      --field-selector="involvedObject.kind=Pod,involvedObject.name=${probe_name}" \
      -o json)

    if jq -e 'any(.items[]?; .type == "Warning" and .reason == "FailedCreatePodSandBox")' \
      <<< "${terminal_event}" >/dev/null; then
      echo "Kata runtime sandbox creation reported a terminal warning for ${runtime_handler}" >&2
      dump_kata_runtime_failure

      return 1
    fi

    sleep 2
  done

  if ! jq -e 'any(.status.conditions[]?; .type == "Ready" and .status == "True")' \
    <<< "${pod_state}" >/dev/null; then
    echo "Kata runtime probe did not become Ready before the five-minute deadline for ${runtime_handler}" >&2
    dump_kata_runtime_failure

    return 1
  fi

  avc_count=$(kata_runtime_avc_count)

  if [[ ${avc_count} -ne 0 ]]; then
    echo "SELinux reported ${avc_count} AVC denials after the Kata runtime probe on ${node_name}" >&2
    dump_kata_runtime_failure

    return 1
  fi

  ${KUBECTL} delete pod "${probe_name}" --wait=true --timeout=2m
  ${KUBECTL} delete runtimeclass "${runtime_class_name}" --wait=true --timeout=2m

  echo "Kata runtime enforcing probe passed on ${node_name}: handler=${runtime_handler} Ready with zero AVC denials"
}
