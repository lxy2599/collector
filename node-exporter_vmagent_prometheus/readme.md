# Edge-Metrics-Collector: 基于 Sidecar 模式的边缘硬件监控系统

## 项目背景
在边缘计算场景下，节点往往带有传感器、ADC、电压计等硬件设备。由于这些设备接口非标（如 I2C 寄存器），标准的 Prometheus Node Exporter 无法直接采集。

本项目通过 **Kubernetes Sidecar 模式**，实现了一套轻量、动态可配置、且对硬件访问友好的监控方案，能够将边缘节点的底层物理状态无缝接入云端 Prometheus 监控体系。

---

## 系统架构与工作原理

项目采用典型的双容器 Pod架构，利用 Kubernetes 的共享卷和主机网络特性。



### 1. 核心流程说明
1.  **策略加载**: `metric-sidecar` (Go 程序) 读取 `nodes.conf`，根据 `NODE_NAME` 动态决定采集脚本（如 `space_server.sh`）和采集频率。
2.  **硬件读取**: Sidecar 周期性调用子进程，脚本通过 `i2c-tools` 穿透容器隔离，直接访问宿主机 `/dev/i2c-X` 接口。
3.  **指标转换**: 脚本输出 `key=value` 格式数据，Go 程序实时解析并映射为 Prometheus `Gauge` 指标。
4.  **原子写入**: 采用“临时文件+原子重命名”策略，将指标写入共享卷下的 `.prom` 文件，供 Node Exporter 扫描。
5.  **可靠推送**: `vmagent` 抓取合并后的指标，通过带缓存的 **Remote Write** 协议推送到 Master 节点的 Prometheus。

---

## 关键技术深度解析

### A. 原子化写入机制 (Data Consistency)
为了防止 `node-exporter` 读取到处于写入中间态的残缺数据，系统实现了原子替换逻辑：
* **Step 1**: 指标先写入 `custom_metrics.prom.tmp.<pid>`。
* **Step 2**: 写入完成后调用 `os.Rename`。在 Linux 内核中，`rename` 是原子操作，确保了指标文件在任何时刻对读取者都是完整合法的。


### B. 边缘端可靠性设计 (Resilience)
* **断网补数**: `vmagent` 开启 `-remoteWrite.tmpDataPath` 持久化队列。当边缘网络波动时，数据自动积压在宿主机磁盘，网络恢复后全量补传。
* **乱序容忍**: Prometheus 服务端配置 `out_of_order_time_window: 1d`，允许接收因断网重传导致的“旧”时间戳数据，确保监控曲线连续不中断。

### C. 全链路时区对齐 (Time Sync)
系统通过挂载宿主机 `/etc/localtime` 并配置环境变量 `TZ=Asia/Shanghai`，实现了 **Sidecar 日志、采集时间戳、Prometheus 存储** 全线对齐北京时间（CST），消除了分布式系统排错中常见的 8 小时偏移偏差。

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
