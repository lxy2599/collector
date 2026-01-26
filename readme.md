# Edge-Metrics-Collector: 基于 Sidecar 模式的边缘硬件监控系统

## 项目背景
在边缘计算场景下，节点往往带有传感器、ADC、电压计等硬件设备。由于这些设备接口非标（如 I2C 寄存器），标准的 Prometheus Node Exporter 无法直接采集。

本项目通过 **Kubernetes Sidecar 模式**，实现了一套轻量、动态可配置、且对硬件访问友好的监控方案，能够将边缘节点的底层物理状态无缝接入云端 Prometheus 监控体系。

---

## 系统架构与工作原理

项目采用典型的双容器 Pod架构，利用 Kubernetes 的共享卷和主机网络特性。



### 1. 核心流程说明
1.  **策略加载**: `metric-sidecar` (Go 程序) 启动时读取 `nodes.conf`，根据当前主机名决定采集脚本（如 `space_server.sh`）和采集频率（如 `10s`）。
2.  **硬件读取**: 按照设定的 `Interval`，Sidecar 调用子进程执行 Shell 脚本。脚本通过 `i2c-tools` 直接穿透容器访问宿主机的 `/dev/i2c-X` 接口。
3.  **指标转换**: 脚本输出 `key=value` 格式数据，Sidecar 解析并映射为 Prometheus 的 `Gauge` 指标。
4.  **原子写入**: Sidecar 将指标原子化地写入共享目录下的 `.prom` 文件。
5.  **指标暴露**: `node-exporter` 的 `textfile` 模块扫描该目录，将自定义指标与系统指标合并，通过 `9100` 端口暴露。

---

## 关键技术深度解析

### A. 原子化写入机制
为了防止 `node-exporter` 在读取指标时，Sidecar 恰好正在写入导致数据断裂，程序采用了 **“临时文件 + 原子重命名”** 的策略：
* **Step 1**: 指标先写入 `custom_metrics.prom.tmp.<pid>.<goroutine>`。
* **Step 2**: 写入完成后调用 `os.Rename`。在 Linux 文件系统中，`rename` 是原子操作，确保了指标文件始终完整，不会出现读取到一半的情况。

### B. 动态频率与策略中心
通过解析 `nodes.conf` 实现了针对不同节点特征的定制化监控策略：
* **高频监控**: 对于核心边缘网关（如 `bupt-a`），设置 `10s` 频率，捕捉瞬时电压波动。
* **空闲/静默模式**: 对于管理节点或非采集节点，Sidecar 进入 **1 小时一次** 的心跳模式，仅打印存活日志，不启动子进程，节省 CPU 与 I/O。

### C. 边缘端优化
针对边缘设备资源受限的特点：
* **低开销**: 使用 Go 静态编译，RSS 常驻内存在 **12MB** 左右。
* **超时闭环**: 使用 `context.WithTimeout` 限制脚本执行时间（默认 10s）。若 I2C 总线因硬件故障卡死，Sidecar 会强制杀死超时进程，防止句柄泄露。
* **环境自包含**: 镜像基于 Alpine，内置原生链接 `musl libc` 的 `i2c-tools`，解决跨发行版运行时的库依赖问题。

# Edge-Metrics-Collector 扩展与维护手册

本手册旨在指导开发者如何在不修改 Go 核心代码的情况下，通过 Kubernetes 配置动态扩展监控能力。系统设计的核心原则是 **“配置即策略”**。

---

## 1. 场景一：在现有脚本中新增指标
如果你需要采集同一硬件上的新数据（例如：在 `space_server.sh` 中增加湿度 `humidity` 采集）：

1.  **修改脚本逻辑**: 在 ConfigMap 的对应脚本块中增加读取命令。
2.  **按格式输出**: 确保输出符合 `key=value` 格式。
    ```bash
    # 示例：在 space_server.sh 中新增逻辑
    humidity_raw=$(i2cget -f -y 0 0x40 0x01 w 2>/dev/null)
    # ... 数据换算逻辑 ...
    echo "humidity=${humidity_val}"
    ```
3.  **自动识别**: Go 程序会自动捕获该输出，并映射为名为 `edge_humidity` 的 Prometheus 指标，无需重启 Go 编译。

---

## 2. 场景二：添加全新的采集脚本
当你接入了不同硬件布局的新节点，需要一套完全不同的 I2C 读取逻辑：

1.  **定义新脚本块**: 在 `all-in-one.yaml` 的 `ConfigMap` 中新增一个文件入口。
2.  **编写 Bash 逻辑**:
    ```yaml
    data:
      jetson_nano.sh: |
        #!/bin/bash
        # 针对 Jetson 硬件的采集逻辑
        gpu_temp=$(cat /sys/class/thermal/thermal_zone1/temp)
        echo "gpu_temperature=$((gpu_temp / 1000))"
    ```
3.  **注意**: 脚本名称必须以 `.sh` 结尾，且开头包含 `#!/bin/bash`。

---

## 3. 场景三：接入新类型的节点
当你向集群添加了新服务器，且希望它执行特定的监控策略：

1.  **确定节点名称**: 获取新节点的 `kubernetes.io/hostname`（例如 `edge-node-01`）。
2.  **更新映射关系**: 在 `nodes.conf` 中建立 `节点=脚本名:频率` 的关联。
    ```text
    # nodes.conf 示例
    bupt-a=space_server:10s
    edge-node-01=jetson_nano:30s  # 指定该节点使用 jetson_nano.sh，30秒采集一次
    ```

---

## 4. 场景四：修改全局默认配置
如果需要修改系统底层逻辑，则需修改 `collector.go` 并重新编译：

| 修改目标 | 位置 (Variable) | 说明 |
| :--- | :--- | :--- |
| **指标名称前缀** | `metricPrefix` | 修改后所有指标将不再以 `edge_` 开头 |
| **存储路径** | `promFile` | 若修改，必须同步修改 DaemonSet 的启动参数 |
| **默认采集频率** | `defaultInterval` | 当 `nodes.conf` 未指定频率时的缺省值（默认 15s） |
| **脚本执行超时** | `context.WithTimeout` | 若硬件响应极慢，可将 10s 调大防止采集被中断 |

---

## 变更生效流程
修改完 `YAML` 或脚本后，请务必执行以下三步以确保边缘端配置更新：

1.  **应用配置**: 
    ```bash
    kubectl apply -f all-in-one.yaml
    ```
2.  **触发重启**: 
    由于 Go 程序在启动时加载一次配置，需触发 DaemonSet 滚动更新：
    ```bash
    kubectl rollout restart daemonset node-exporter-arm64 -n monitoring
    ```
3.  **状态校验**:
    ```bash
    # 查看 Sidecar 日志确认配置加载
    kubectl logs -l app=node-exporter -n monitoring -c metric-sidecar --tail=10
    ```

---

## 💡 开发小贴士
* **原子性保证**: Go 程序会自动处理文件的原子替换，你只需保证脚本输出的 `key` 只包含字母、数字和下划线。
* **低功耗运行**: 对于不需要监控硬件的节点，在 `nodes.conf` 中将其设为 `default` 类型即可。
* **脚本调试**: 在写入 ConfigMap 前，建议先在宿主机终端直接运行脚本，确保输出结果符合 `key=value`。
