#!/usr/bin/env bash
#
# nextcloud-server-setup.sh — server-side enablement for nextcloud-sync-daemon
# event sources (Refs #36).
#
# Run this ON THE NEXTCLOUD SERVER (not the sync client). It automates the
# manual steps documented in the daemon README:
#
#   notify-push   install/enable the notify_push app and run its self-test
#   webhook       register the four webhook_listeners events pointing at
#                 your daemon's webhook listener
#   webhook-list  show currently registered webhooks
#
# The admin password is never passed on a command line: it is read from the
# NC_ADMIN_PASS environment variable or prompted for, and handed to curl via
# a config file on stdin.
#
# Examples:
#   sudo ./nextcloud-server-setup.sh notify-push --push-url https://cloud.example.com/push
#   sudo ./nextcloud-server-setup.sh webhook \
#       --nextcloud-url https://cloud.example.com \
#       --admin admin \
#       --daemon-url http://192.0.2.10:8767/ \
#       --sync-user sync-user \
#       --secret "$(cat /path/to/webhook-secret)"
#
set -euo pipefail

OCC="${OCC:-sudo -u www-data php /var/www/nextcloud/occ}"
DRY_RUN=0

usage() {
    sed -n '2,/^set -euo/p' "$0" | sed 's/^# \{0,1\}//' | head -n -1
    cat <<EOF
Global options (before the command):
  --occ CMD       occ invocation (default: "$OCC"; also via \$OCC)
  --dry-run       print the commands/requests instead of executing them

Command options:
  notify-push:  --push-url URL       public URL of the push server (proxy route)
  webhook:      --nextcloud-url URL  base URL of the Nextcloud server
                --admin USER         admin username (password: \$NC_ADMIN_PASS or prompt)
                --daemon-url URL     the daemon's webhook listener, e.g. http://HOST:8767/
                --sync-user USER     Nextcloud user whose file events to forward
                --secret SECRET      must match webhook.secret in the daemon config
  webhook-list: --nextcloud-url URL  --admin USER
EOF
}

run() {
    if [ "$DRY_RUN" = 1 ]; then
        echo "DRY-RUN: $*"
    else
        eval "$*"
    fi
}

# curl_admin METHOD URL [curl args...] — authenticated OCS request with the
# admin credential supplied via stdin config, never argv.
curl_admin() {
    local method=$1 url=$2
    shift 2
    if [ "$DRY_RUN" = 1 ]; then
        echo "DRY-RUN: curl -X $method $url $* (as $ADMIN_USER, password via stdin config)"
        return 0
    fi
    curl -sS -K - -X "$method" \
        -H "OCS-APIRequest: true" \
        "$@" "$url" <<EOF
user = "${ADMIN_USER}:${ADMIN_PASS}"
EOF
}

require() {
    local val=$1 name=$2
    if [ -z "$val" ]; then
        echo "error: $name is required" >&2
        usage >&2
        exit 1
    fi
}

read_admin_pass() {
    if [ -n "${NC_ADMIN_PASS:-}" ]; then
        ADMIN_PASS="$NC_ADMIN_PASS"
    elif [ "$DRY_RUN" = 1 ]; then
        ADMIN_PASS="(dry-run)"
    else
        read -r -s -p "Nextcloud admin password for ${ADMIN_USER}: " ADMIN_PASS
        echo >&2
    fi
}

cmd_notify_push() {
    local push_url=""
    while [ $# -gt 0 ]; do
        case $1 in
            --push-url) push_url=$2; shift 2 ;;
            *) echo "error: unknown option $1" >&2; exit 1 ;;
        esac
    done
    require "$push_url" "--push-url"

    echo "== Installing/enabling the notify_push app"
    if $OCC app:list --output=plain 2>/dev/null | grep -q "notify_push"; then
        run "$OCC app:enable notify_push"
    else
        run "$OCC app:install notify_push"
    fi

    cat <<EOF

== Manual steps this script cannot do for you
1. Run the push server binary the app ships (or its container). See:
   https://github.com/nextcloud/notify_push#setup
2. Add a reverse-proxy route from ${push_url} to the push server (port 7867
   by default).
   Note for Docker installs: trusted_proxies must be IP/CIDR ranges —
   container hostnames do not resolve for Nextcloud's proxy-trust check.

== Self-test (verifies proxy, trusted_proxies, and the push server)
EOF
    run "$OCC notify_push:setup '$push_url'"

    cat <<EOF

On the daemon host, set in the config:
    notify_push:
      enabled: true
and check the daemon log for "notify_push connected and authenticated".
EOF
}

WEBHOOK_EVENTS="NodeCreatedEvent NodeWrittenEvent NodeDeletedEvent NodeRenamedEvent"

cmd_webhook() {
    local nc_url="" daemon_url="" sync_user="" secret=""
    ADMIN_USER=""
    while [ $# -gt 0 ]; do
        case $1 in
            --nextcloud-url) nc_url=$2; shift 2 ;;
            --admin)         ADMIN_USER=$2; shift 2 ;;
            --daemon-url)    daemon_url=$2; shift 2 ;;
            --sync-user)     sync_user=$2; shift 2 ;;
            --secret)        secret=$2; shift 2 ;;
            *) echo "error: unknown option $1" >&2; exit 1 ;;
        esac
    done
    require "$nc_url" "--nextcloud-url"
    require "$ADMIN_USER" "--admin"
    require "$daemon_url" "--daemon-url"
    require "$sync_user" "--sync-user"
    require "$secret" "--secret"
    read_admin_pass

    echo "== Enabling the webhook_listeners app"
    run "$OCC app:enable webhook_listeners"

    echo "== Registering ${WEBHOOK_EVENTS// /, }"
    local event
    for event in $WEBHOOK_EVENTS; do
        curl_admin POST "${nc_url%/}/ocs/v2.php/apps/webhook_listeners/api/v1/webhooks?format=json" \
            -H "Content-Type: application/json" \
            -d "{\"httpMethod\":\"POST\",\"uri\":\"${daemon_url}\",\"event\":\"OCP\\\\Files\\\\Events\\\\Node\\\\${event}\",\"userIdFilter\":\"${sync_user}\",\"headers\":{\"X-Webhook-Secret\":\"${secret}\"}}"
        echo
    done

    cat <<EOF

On the daemon host, set in the config:
    webhook:
      enabled: true
      listen: 0.0.0.0:8767
      secret: <the same secret>
The daemon host must be reachable from this server at ${daemon_url}.
If it is not (NAT, firewall), use notify_push instead — it needs no
inbound port.
EOF
}

cmd_webhook_list() {
    local nc_url=""
    ADMIN_USER=""
    while [ $# -gt 0 ]; do
        case $1 in
            --nextcloud-url) nc_url=$2; shift 2 ;;
            --admin)         ADMIN_USER=$2; shift 2 ;;
            *) echo "error: unknown option $1" >&2; exit 1 ;;
        esac
    done
    require "$nc_url" "--nextcloud-url"
    require "$ADMIN_USER" "--admin"
    read_admin_pass

    curl_admin GET "${nc_url%/}/ocs/v2.php/apps/webhook_listeners/api/v1/webhooks?format=json"
    echo
}

main() {
    while [ $# -gt 0 ]; do
        case $1 in
            --occ)     OCC=$2; shift 2 ;;
            --dry-run) DRY_RUN=1; shift ;;
            -h|--help) usage; exit 0 ;;
            notify-push)  shift; cmd_notify_push "$@"; exit 0 ;;
            webhook)      shift; cmd_webhook "$@"; exit 0 ;;
            webhook-list) shift; cmd_webhook_list "$@"; exit 0 ;;
            *) echo "error: unknown command or option: $1" >&2; usage >&2; exit 1 ;;
        esac
    done
    usage >&2
    exit 1
}

main "$@"
