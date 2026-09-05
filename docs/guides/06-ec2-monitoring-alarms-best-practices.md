# EC2 Monitoring, Alarms, and Safety Best Practices

How to get warned *before* an EC2 server runs out of memory, fills its disk, or otherwise gets
into trouble — rather than finding out when a customer reports a failed PDF. This applies to any
EC2 instance, not just one running this service, but every example and threshold below is anchored
to this project's own real, documented numbers (`docs/planning/FINALISATION-REPORT.md`,
`docs/research/load-test-results.md`) so it's directly usable, not generic filler.

## The one thing to understand first

**EC2's default ("basic") monitoring does not include memory or disk usage at all.** This surprises
almost everyone the first time they go looking for a memory alarm and can't find the metric. What
EC2 gives you for free, no setup required:

| Metric | What it tells you |
|---|---|
| `CPUUtilization` | CPU load |
| `NetworkIn` / `NetworkOut` | Network traffic |
| `DiskReadOps` / `DiskWriteOps` | Disk I/O *activity* (not how full the disk is) |
| `StatusCheckFailed_System` / `StatusCheckFailed_Instance` | Whether the underlying hardware/hypervisor or the instance's own OS/network stack has failed |
| `CPUCreditBalance` (T-family only — `t2`/`t3`/`t4g`) | How many burst CPU credits are left |

**None of these tell you memory usage or disk space used.** Both require installing the **CloudWatch
Agent** on the instance — this is not optional if "alert before memory becomes full" is the goal,
and it's the single most important step in this guide.

---

## 1. Give the instance permission to report its own metrics

The CloudWatch Agent needs an IAM role attached to the instance (not IAM user credentials on disk —
an instance role is safer and doesn't need key rotation).

**Console path**: EC2 → select your instance → **Actions → Security → Modify IAM role** → attach a
role with the AWS-managed policy `CloudWatchAgentServerPolicy` (create the role first if none
exists, via IAM → Roles → Create role → EC2 as the trusted entity). This can be done on a running
instance, no reboot needed.

**CLI equivalent**:
```bash
aws iam create-role --role-name EC2-CloudWatchAgent \
  --assume-role-policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}'
aws iam attach-role-policy --role-name EC2-CloudWatchAgent \
  --policy-arn arn:aws:iam::aws:policy/CloudWatchAgentServerPolicy
aws iam create-instance-profile --instance-profile-name EC2-CloudWatchAgent
aws iam add-role-to-instance-profile --instance-profile-name EC2-CloudWatchAgent --role-name EC2-CloudWatchAgent
aws ec2 associate-iam-instance-profile --instance-id <instance-id> \
  --iam-instance-profile Name=EC2-CloudWatchAgent
```

---

## 2. Install and configure the CloudWatch Agent

```bash
wget https://s3.amazonaws.com/amazoncloudwatch-agent/ubuntu/amd64/latest/amazon-cloudwatch-agent.deb
sudo dpkg -i amazon-cloudwatch-agent.deb
```

Write its configuration — this is the part that decides *which* metrics get collected. The
defaults collect nothing beyond what EC2 already gives you for free, so this file is what actually
turns on memory/disk visibility:

```bash
sudo tee /opt/aws/amazon-cloudwatch-agent/bin/config.json >/dev/null <<'EOF'
{
  "agent": { "metrics_collection_interval": 60, "run_as_user": "root" },
  "metrics": {
    "namespace": "CWAgent",
    "append_dimensions": { "InstanceId": "${aws:InstanceId}" },
    "metrics_collected": {
      "mem":  { "measurement": ["mem_used_percent"] },
      "swap": { "measurement": ["swap_used_percent"] },
      "disk": { "measurement": ["used_percent"], "resources": ["/"] }
    }
  },
  "logs": {
    "logs_collected": {
      "files": {
        "collect_list": [
          {
            "file_path": "/var/log/great-pdf-generator/app.log",
            "log_group_name": "great-pdf-generator",
            "log_stream_name": "{instance_id}"
          }
        ]
      }
    }
  }
}
EOF

sudo /opt/aws/amazon-cloudwatch-agent/bin/amazon-cloudwatch-agent-ctl \
  -a fetch-config -m ec2 -s -c file:/opt/aws/amazon-cloudwatch-agent/bin/config.json
```

The `logs` section ships this service's own logs to CloudWatch Logs for searching and alerting
(§7) — but it needs the service to actually be writing to that file path, which it isn't by
default (it logs to stdout/journald). Add one line to the systemd unit to redirect it:

```ini
# in /etc/systemd/system/great-pdf-generator.service, under [Service]:
StandardOutput=append:/var/log/great-pdf-generator/app.log
```

```bash
sudo mkdir -p /var/log/great-pdf-generator
sudo chown pdfsvc:pdfsvc /var/log/great-pdf-generator
sudo systemctl daemon-reload && sudo systemctl restart great-pdf-generator
```

Since this file now grows forever on its own, give it a rotation policy (this is the same disk
hygiene principle as `docs/guides/05-small-vm-production-and-migration-guide.md` §2.3):

```bash
sudo tee /etc/logrotate.d/great-pdf-generator >/dev/null <<'EOF'
/var/log/great-pdf-generator/app.log {
    daily
    rotate 7
    compress
    missingok
    notifempty
    copytruncate
}
EOF
```

**Verify it's actually working** before building alarms on top of it:
```bash
aws cloudwatch list-metrics --namespace CWAgent
```
You should see `mem_used_percent`, `swap_used_percent`, and `disk_used_percent` listed within a
minute or two of the agent starting.

---

## 3. Create an SNS topic for notifications

One topic, subscribed to however you want to be told — email is simplest to start:

```bash
aws sns create-topic --name ec2-alerts
# note the TopicArn it prints, you'll reuse it below

aws sns subscribe --topic-arn <TopicArn> --protocol email --notification-endpoint you@example.com
```
Check your inbox and click the confirmation link — a subscription stays in `PendingConfirmation`
and delivers nothing until confirmed. SMS (`--protocol sms --notification-endpoint +1...`) and
Slack (via a small Lambda subscribed to the same topic, forwarding to a Slack webhook URL) work
the same way, on the same topic.

---

## 4. The recommended alarm set

| # | Metric | Namespace | Threshold | Why this threshold |
|---|---|---|---|---|
| 1 | `mem_used_percent` | `CWAgent` | Warn at **75%**, critical at **90%**, sustained 3 periods | This service's own systemd `MemoryMax` already hard-kills it before the *whole box* runs out — this alarm exists to catch the climb *before* that kill happens, so you find out from CloudWatch, not from a customer |
| 2 | `disk_used_percent` | `CWAgent` | Warn at **80%** | A full disk stops logging, stops the package manager, and can wedge the OS itself — always the more urgent of the two to catch early |
| 3 | `swap_used_percent` | `CWAgent` | Warn at **50%** | Rising swap usage on a small VM is the leading indicator of the exact "server getting full" symptom described in earlier diagnosis — it degrades latency badly well before an actual crash |
| 4 | `CPUCreditBalance` (T-family instances only) | `AWS/EC2` | Alarm when **< 50** | Burstable instances (`t2`/`t3`/`t4g`) throttle hard once credits run out — this looks exactly like "the server suddenly got slow," and it's invisible unless you specifically watch this metric |
| 5 | `StatusCheckFailed_System` | `AWS/EC2` | **> 0** for 3 periods, with an **auto-recovery action** | Underlying hardware/hypervisor fault — this one you can auto-remediate, not just get notified about (see below) |
| 6 | `StatusCheckFailed_Instance` | `AWS/EC2` | **> 0** for 3 periods | The instance's own OS/network stack has stopped responding — usually needs a manual reboot, not auto-recoverable the same way |

### Worked example: the memory alarm

```bash
aws cloudwatch put-metric-alarm \
  --alarm-name "pdfsvc-memory-high" \
  --namespace "CWAgent" \
  --metric-name "mem_used_percent" \
  --dimensions Name=InstanceId,Value=<instance-id> \
  --statistic Average \
  --period 60 \
  --evaluation-periods 3 \
  --threshold 90 \
  --comparison-operator GreaterThanThreshold \
  --alarm-actions <TopicArn>
```

### Worked example: disk space

```bash
aws cloudwatch put-metric-alarm \
  --alarm-name "pdfsvc-disk-high" \
  --namespace "CWAgent" \
  --metric-name "disk_used_percent" \
  --dimensions Name=InstanceId,Value=<instance-id> Name=path,Value=/ Name=fstype,Value=ext4 \
  --statistic Average \
  --period 300 \
  --evaluation-periods 1 \
  --threshold 80 \
  --comparison-operator GreaterThanThreshold \
  --alarm-actions <TopicArn>
```
(The exact `fstype` dimension value depends on your filesystem — check with `aws cloudwatch
list-metrics --namespace CWAgent --metric-name disk_used_percent` and copy the dimensions it
actually reports.)

### Worked example: CPU credit balance (burstable/T-family instances)

```bash
aws cloudwatch put-metric-alarm \
  --alarm-name "pdfsvc-cpu-credits-low" \
  --namespace "AWS/EC2" \
  --metric-name "CPUCreditBalance" \
  --dimensions Name=InstanceId,Value=<instance-id> \
  --statistic Average \
  --period 300 \
  --evaluation-periods 1 \
  --threshold 50 \
  --comparison-operator LessThanThreshold \
  --alarm-actions <TopicArn>
```
No agent needed for this one — `CPUCreditBalance` is part of EC2's free basic monitoring for
T-family instances.

### Worked example: status check failure with automatic recovery

This one doesn't just notify — it tells AWS to actually recover the instance (migrate it to new
hardware and reboot it), which is the one case in this whole guide where "the alarm fixes it for
you" is real, not aspirational:

```bash
aws cloudwatch put-metric-alarm \
  --alarm-name "pdfsvc-status-check-failed" \
  --namespace "AWS/EC2" \
  --metric-name "StatusCheckFailed_System" \
  --dimensions Name=InstanceId,Value=<instance-id> \
  --statistic Maximum \
  --period 60 \
  --evaluation-periods 3 \
  --threshold 0 \
  --comparison-operator GreaterThanThreshold \
  --alarm-actions arn:aws:automate:<region>:ec2:recover <TopicArn>
```
`arn:aws:automate:<region>:ec2:recover` is a special built-in action ARN, not a real SNS topic —
you can pass both it and your real topic ARN in the same `--alarm-actions` list, so you get
notified *and* the instance gets recovered. Only works on instance types/configurations that
support the recovery action (most current-generation EBS-backed instances, including `t3`, do).

---

## 5. Application-level health: watching *this service's own* signals

The alarms above catch OS/hardware-level problems. This service already exposes richer signals
about its own health that generic EC2 monitoring can't see — `docs/guides/04-deployment.md`
specifically calls out **`restart_attempts` climbing while `restarts` stays flat** as "the field to
alert on": it means Chromium can't start at all (bad path, out-of-memory, full disk), and no amount
of the automatic self-healing this service already does will fix it.

A small script + cron job turns `/readyz`'s JSON into CloudWatch custom metrics:

```bash
sudo tee /usr/local/bin/pdfsvc-health-metric.sh >/dev/null <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

TOKEN=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" \
  -H "X-aws-ec2-metadata-token-ttl-seconds: 21600")
REGION=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" \
  http://169.254.169.254/latest/meta-data/placement/region)

STATUS=$(curl -s -o /tmp/readyz.json -w "%{http_code}" http://localhost:8080/readyz)
HEALTHY=0; [ "$STATUS" = "200" ] && HEALTHY=1

aws cloudwatch put-metric-data --region "$REGION" --namespace "GreatPdfGenerator" \
  --metric-name "ServiceHealthy" --value "$HEALTHY" --unit Count

if [ -s /tmp/readyz.json ] && command -v jq >/dev/null; then
  RESTARTS=$(jq -r '.chromium.restarts // 0' /tmp/readyz.json)
  ATTEMPTS=$(jq -r '.chromium.restart_attempts // 0' /tmp/readyz.json)
  HANGS=$(jq -r '.chromium.hangs_detected // 0' /tmp/readyz.json)
  aws cloudwatch put-metric-data --region "$REGION" --namespace "GreatPdfGenerator" \
    --metric-name "RestartAttemptsMinusRestarts" --value "$((ATTEMPTS - RESTARTS))" --unit Count
  aws cloudwatch put-metric-data --region "$REGION" --namespace "GreatPdfGenerator" \
    --metric-name "HangsDetected" --value "$HANGS" --unit Count
fi
EOF
sudo chmod +x /usr/local/bin/pdfsvc-health-metric.sh
sudo apt install -y jq   # if not already installed

# Run every minute:
(crontab -l 2>/dev/null; echo "* * * * * /usr/local/bin/pdfsvc-health-metric.sh >>/var/log/pdfsvc-health-metric.log 2>&1") | crontab -
```

Then alarm on what it publishes:

```bash
aws cloudwatch put-metric-alarm \
  --alarm-name "pdfsvc-not-ready" \
  --namespace "GreatPdfGenerator" --metric-name "ServiceHealthy" \
  --statistic Minimum --period 60 --evaluation-periods 3 \
  --threshold 1 --comparison-operator LessThanThreshold \
  --alarm-actions <TopicArn>

aws cloudwatch put-metric-alarm \
  --alarm-name "pdfsvc-chromium-cannot-start" \
  --namespace "GreatPdfGenerator" --metric-name "RestartAttemptsMinusRestarts" \
  --statistic Maximum --period 300 --evaluation-periods 2 \
  --threshold 5 --comparison-operator GreaterThanThreshold \
  --alarm-actions <TopicArn>
```

The second alarm is specifically the "no amount of retrying fixes this, a human needs to look"
signal this project's own documentation names — a rising gap between attempts and successful
restarts means Chromium is being asked to start and can't (bad `CHROMIUM_PATH`, disk full, OOM),
not a transient blip that self-heals.

---

## 6. Alerting on the logs themselves

Once logs are flowing into CloudWatch Logs (§2), you can alert on patterns inside the JSON this
service already emits, without changing any application code. A **metric filter** turns a log
pattern into a number CloudWatch can alarm on:

```bash
aws logs put-metric-filter \
  --log-group-name "great-pdf-generator" \
  --filter-name "server-errors" \
  --filter-pattern '{ $.level = "ERROR" }' \
  --metric-transformations \
    metricName=ServerErrorCount,metricNamespace=GreatPdfGenerator,metricValue=1

aws cloudwatch put-metric-alarm \
  --alarm-name "pdfsvc-error-rate-high" \
  --namespace "GreatPdfGenerator" --metric-name "ServerErrorCount" \
  --statistic Sum --period 300 --evaluation-periods 1 \
  --threshold 10 --comparison-operator GreaterThanThreshold \
  --alarm-actions <TopicArn>
```
Recall from `docs/guides/04-deployment.md` that this service deliberately logs `503`s at `warn`,
not `error` — a designed load-shedding response, not a fault — so filtering on `ERROR` specifically
(not `WARN`) keeps this alarm meaningful instead of firing on ordinary backpressure.

---

## 7. Test your alarms — don't assume they fire

An alarm nobody has seen trigger is a guess, not a safety net. Before trusting any of the above in
production, force each condition once and confirm the notification actually arrives:

```bash
# Memory pressure (stress-ng is safe to install temporarily for this one test)
sudo apt install -y stress-ng
stress-ng --vm 1 --vm-bytes 90% --timeout 240s

# Disk pressure
fallocate -l 3G /tmp/filler.img   # size this to whatever pushes your disk past its threshold
rm /tmp/filler.img                 # clean up immediately after confirming the alarm fired

# Service down (confirms both the systemd auto-restart AND the ServiceHealthy alarm)
sudo systemctl stop great-pdf-generator
sleep 90
sudo systemctl start great-pdf-generator
```
Watch `aws cloudwatch describe-alarms --alarm-names <name>` transition through `INSUFFICIENT_DATA`
→ `ALARM` → `OK`, and confirm the SNS notification actually lands in your inbox each time.

---

## 8. Broader EC2 safety best practices (beyond alarms)

A short checklist worth doing once, independent of the alerting setup above:

| Practice | Why |
|---|---|
| **Billing alarm** on estimated charges (`AWS/Billing` namespace, `us-east-1` only, must be enabled in account billing preferences first) | Catches a runaway instance/resource before the invoice does |
| **Enforce IMDSv2** (`aws ec2 modify-instance-metadata-options --http-tokens required`) | Blocks the classic SSRF-to-credential-theft path through the instance metadata service |
| **Security group hygiene** | Already covered for this specific service in `docs/guides/05-small-vm-production-and-migration-guide.md` §4 — never `0.0.0.0/0` on an unauthenticated port |
| **Automated EBS snapshots** (AWS Backup, or Data Lifecycle Manager) | A monitoring alarm tells you something is wrong; a snapshot is what lets you actually recover from it |
| **Unattended security upgrades** (`sudo apt install unattended-upgrades`) | The "60 updates can be applied immediately" banner on a fresh instance is exactly what this closes going forward |
| **Tag every resource** (`Name`, `Environment`, `Owner`) | Makes the difference between finding the right instance in an emergency and guessing |

---

## Quick reference

| Want to know about... | Metric | Namespace | Needs agent? |
|---|---|---|---|
| Memory | `mem_used_percent` | `CWAgent` | Yes |
| Swap | `swap_used_percent` | `CWAgent` | Yes |
| Disk space | `disk_used_percent` | `CWAgent` | Yes |
| CPU load | `CPUUtilization` | `AWS/EC2` | No |
| Burst credits (T-family) | `CPUCreditBalance` | `AWS/EC2` | No |
| Hardware/hypervisor health | `StatusCheckFailed_System` | `AWS/EC2` | No |
| OS/network health | `StatusCheckFailed_Instance` | `AWS/EC2` | No |
| This service's own readiness | `ServiceHealthy` (custom) | `GreatPdfGenerator` | Small script (§5) |
| Chromium unable to start | `RestartAttemptsMinusRestarts` (custom) | `GreatPdfGenerator` | Small script (§5) |
| Error rate from logs | `ServerErrorCount` (metric filter) | `GreatPdfGenerator` | Log shipping (§2, §6) |
