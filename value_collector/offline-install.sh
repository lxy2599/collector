#!/bin/bash

# Copyright 2021 The KubeEdge Authors.
# Modified for non-hostNetwork deployment with hostPort mapping

set -o errexit
set -o nounset
set -o pipefail

TMP_DIR='/opt/sedna'
SEDNA_ROOT=${SEDNA_ROOT:-$TMP_DIR}
DEFAULT_SEDNA_VERSION=v0.7.0

# -----------------------------------------------------------
# 基础工具函数
# -----------------------------------------------------------

get_latest_version() {
  local repo=kubeedge/sedna
  {
    curl -s https://api.github.com/repos/$repo/releases/latest |
    awk '/"tag_name":/&&$0=$2' |
    sed 's/[",]//g'
  } || echo $DEFAULT_SEDNA_VERSION
}

: ${SEDNA_VERSION:=$(get_latest_version)}
SEDNA_VERSION=v${SEDNA_VERSION#v}

_download_yamls() {
  yaml_dir=$1
  mkdir -p ${SEDNA_ROOT}/$yaml_dir
  cd ${SEDNA_ROOT}/$yaml_dir
  for yaml in ${yaml_files[@]}; do
    [ -e "$yaml" ] && continue
    echo downloading $yaml into ${SEDNA_ROOT}/$yaml_dir
    local try_times=30 i=1 timeout=2
    while ! timeout ${timeout}s curl -sSO https://raw.githubusercontent.com/kubeedge/sedna/main/$yaml_dir/$yaml; do
      ((++i>try_times)) && { echo timeout to download $yaml; exit 2; }
      echo -en "retrying to download $yaml after $[i*timeout] seconds...\r"
    done
  done
}

download_yamls() {
  yaml_files=(sedna.io_datasets.yaml sedna.io_federatedlearningjobs.yaml sedna.io_incrementallearningjobs.yaml sedna.io_jointinferenceservices.yaml sedna.io_lifelonglearningjobs.yaml sedna.io_models.yaml)
  _download_yamls build/crds
  yaml_files=(gm.yaml)
  _download_yamls build/gm/rbac
}

prepare_install(){
  kubectl create ns sedna || true
}

create_crds() {
  cd ${SEDNA_ROOT}
  kubectl create -f build/crds || true
}

# -----------------------------------------------------------
# 组件创建函数 (核心修改部分)
# -----------------------------------------------------------

# 1. Knowledge Base (KB) - 使用 hostPort 保持 9020
create_kb(){
  cd ${SEDNA_ROOT}
  kubectl $action -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: kb
  namespace: sedna
spec:
  selector:
    sedna: kb
  ports:
    - protocol: TCP
      port: 9020
      targetPort: 9020
      name: "tcp-0"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: kb
  namespace: sedna
spec:
  replicas: 1
  selector:
    matchLabels:
      sedna: kb
  template:
    metadata:
      labels:
        sedna: kb
    spec:
      dnsPolicy: ClusterFirst
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
            - matchExpressions:
              - key: node-role.kubernetes.io/edge
                operator: DoesNotExist
      serviceAccountName: sedna
      containers:
      - name: kb
        image: kubeedge/sedna-kb:$SEDNA_VERSION
        ports:
          - containerPort: 9020
            hostPort: 9020 # 宿主机端口保持 9020
        env:
          - name: KB_URL
            value: "sqlite:///db/kb.sqlite3"
        volumeMounts:
          - name: kb-url
            mountPath: /db
      volumes:
        - name: kb-url
          hostPath:
            path: /opt/kb-data
            type: DirectoryOrCreate
EOF
}

# 2. Global Manager (GM) - 使用 hostPort 保持 9000 & NodePort 30000
create_gm() {
  cd ${SEDNA_ROOT}
  kubectl create -f build/gm/rbac/ || true

  # 准备 ConfigMap，LC 将通过 Service 域名连接 KB
  cat > ${SEDNA_ROOT}/gm.yaml << EOF
kubeConfig: ""
master: ""
namespace: ""
websocket:
  address: 0.0.0.0
  port: 9000
localController:
  server: http://localhost:9100
knowledgeBaseServer:
  server: http://kb.sedna.svc.cluster.local:9020
EOF
  kubectl $action -n sedna configmap gm-config --from-file=${SEDNA_ROOT}/gm.yaml || true

  kubectl $action -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: gm
  namespace: sedna
spec:
  selector:
    sedna: gm
  type: NodePort
  ports:
    - protocol: TCP
      port: 9000
      targetPort: 9000
      nodePort: 30000 # 保持 NodePort 30000
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gm
  namespace: sedna
spec:
  replicas: 1
  selector:
    matchLabels:
      sedna: gm
  template:
    metadata:
      labels:
        sedna: gm
    spec:
      dnsPolicy: ClusterFirst
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
            - matchExpressions:
              - key: node-role.kubernetes.io/edge
                operator: DoesNotExist
      serviceAccountName: sedna
      containers:
      - name: gm
        image: kubeedge/sedna-gm:$SEDNA_VERSION
        command: ["sedna-gm", "--config", "/config/gm.yaml", "-v2"]
        ports:
          - containerPort: 9000
            hostPort: 9000 # 宿主机端口保持 9000
        volumeMounts:
          - name: gm-config
            mountPath: /config
      volumes:
        - name: gm-config
          configMap:
            name: gm-config
EOF
}

# 3. Local Controller (LC) - 使用域名连接 GM，hostPort 保持 9100
create_lc() {
  # 边缘节点通过域名访问 GM，EdgeMesh 负责解析
  local GM_FQDN="gm.sedna.svc.cluster.local:9000"

  kubectl $action -f- <<EOF
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: lc
  namespace: sedna
spec:
  selector:
    matchLabels:
      sedna: lc
  template:
    metadata:
      labels:
        sedna: lc
    spec:
      dnsPolicy: ClusterFirst
      containers:
        - name: lc
          image: kubeedge/sedna-lc:$SEDNA_VERSION
          env:
            - name: GM_ADDRESS
              value: "$GM_FQDN"
            - name: BIND_PORT
              value: "9100"
            - name: NODENAME
              valueFrom:
                fieldRef:
                  fieldPath: spec.nodeName
            - name: ROOTFS_MOUNT_DIR
              value: /rootfs
          ports:
            - containerPort: 9100
              hostPort: 9100 # 宿主机端口保持 9100
          volumeMounts:
            - name: localcontroller
              mountPath: /rootfs
      volumes:
        - name: localcontroller
          hostPath:
            path: /
      restartPolicy: Always
EOF
}

# -----------------------------------------------------------
# 流程控制
# -----------------------------------------------------------

wait_ok() {
  echo "Waiting Sedna components to be ready..."
  kubectl -n sedna wait --for=condition=available --timeout=300s deployment/gm || true
  kubectl -n sedna get pod
}

do_check() {
  action=${SEDNA_ACTION:-create}
}

do_check
case "$action" in
  create)
    echo "Installing Sedna $SEDNA_VERSION (Non-HostNetwork Mode)..."
    prepare_install
    download_yamls
    create_crds
    create_kb
    create_gm
    create_lc
    wait_ok
    ;;
  delete)
    kubectl delete ns sedna --ignore-not-found
    echo "Sedna uninstalled."
    ;;
esac