#!/bin/sh
# Hard ceiling for rsyslog files, run every 15 minutes by log-cap.timer.
# A one-off kernel/systemd flood once left 17G archives in /var/log for
# weeks (logrotate only bounds the live file, never lingering archives),
# which fed a DiskPressure eviction loop for two days.
set -u

purge() {
  find /var/log -xdev -type f \( -name "*.[0-9]" -o -name "*.[0-9].gz" \) -size +200M -delete
}

# 1) No rotated archive may exceed 200M, no matter how old.
purge

# 2) A live file growing past 1G between hourly rotations gets rotated now.
for f in syslog kern.log daemon.log auth.log; do
  [ -f "/var/log/$f" ] || continue
  if [ "$(stat -c%s "/var/log/$f")" -gt 1073741824 ]; then
    logrotate -f /etc/logrotate.d/rsyslog
    purge
    break
  fi
done
