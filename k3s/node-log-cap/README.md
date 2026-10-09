# Node log volume caps

Hard ceiling on per-node log volume. Rationale (2026-10-08): a one-off
kernel/systemd message flood on the Strata host (`burst6`) left two 17.8G
rotated files (`syslog.1`, `kern.log.1`) in `/var/log`. `logrotate`'s `size`
option only bounds the *live* file — a giant archive persists forever when no
later rotation happens to shift it out — so 35.6G of dead logs sat on disk for
weeks, fed a kubelet `DiskPressure` eviction loop for two days, and silently
killed the node's log shipping and node metrics (the monitoring gap this
directory closes).

Three layers, applied to every node:

1. **journald** — `99-disk-cap.conf` (drop-in, `SystemMaxUse=1G`).
2. **rsyslog archives** — `log-cap.timer` runs `log-cap.sh` every 15 minutes:
   rotated archives over 200M are deleted; a live file over 1G is force-rotated
   immediately (then the archive purge applies).
3. **logrotate itself** — `/var/log` is `drwxrwxr-x root syslog`, which trips
   logrotate's insecure-parent check on some machines ("parent directory has
   insecure permissions", rotation silently skipped since 2026-09-13 on the
   L20 node). Fix: add `su root adm` inside the stanza in
   `/etc/logrotate.d/rsyslog`.

## Install (per node)

```sh
scp log-cap.sh log-cap.service log-cap.timer 99-disk-cap.conf <node>:/tmp/
ssh <node> '
  install -m 755 /tmp/log-cap.sh /usr/local/sbin/log-cap.sh
  install -m 644 /tmp/log-cap.service /etc/systemd/system/log-cap.service
  install -m 644 /tmp/log-cap.timer   /etc/systemd/system/log-cap.timer
  install -D -m 644 /tmp/99-disk-cap.conf /etc/systemd/journald.conf.d/99-disk-cap.conf
  grep -qF "su root adm" /etc/logrotate.d/rsyslog || sed -i "s/^{$/{\n\tsu root adm/" /etc/logrotate.d/rsyslog
  systemctl daemon-reload && systemctl enable --now log-cap.timer
  systemctl restart systemd-journald
'
```
