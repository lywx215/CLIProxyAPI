#!/usr/bin/env bash
# Configure only this application's virtual host in an existing BaoTa Nginx.
set -euo pipefail
umask 077
root=${1:?Missing deployment directory}
project=${2:?Missing project}
domain=${3:?Missing domain}
port=${4:?Missing port}
fail() { printf 'HTTPS setup error: %s\n' "$*" >&2; exit 1; }
[[ $root =~ ^/[a-zA-Z0-9_./-]+$ && $project =~ ^[a-z][a-z0-9_-]{0,47}$ ]] || fail 'Invalid deployment path or project.'
[[ $domain =~ ^[a-z0-9][a-z0-9.-]*\.[a-z0-9]+$ && $domain != *..* ]] || fail 'Invalid domain.'
[[ $port =~ ^[0-9]+$ && $port -gt 0 && $port -le 65535 ]] || fail 'Invalid port.'
nginx=/www/server/nginx/sbin/nginx
vhost=/www/server/panel/vhost/nginx/$domain.conf
[[ -x $nginx && -d $(dirname "$vhost") ]] || fail 'An existing BaoTa Nginx installation is required.'
[[ ! -L $vhost ]] || fail 'Virtual host path cannot be a symlink.'
marker="# Managed by CLIProxyAPI VPS deployment: $project"
connection_var=cpa_$(printf '%s' "$domain" | tr '.-' '__')_connection
if [[ -f $vhost ]] && ! grep -Fxq "$marker" "$vhost"; then
    fail "An existing unmanaged virtual host already uses $domain; configure it in BaoTa instead."
fi
"$nginx" -t
mkdir -p "$root/acme-webroot/.well-known/acme-challenge" "$root/tls" "$root/nginx-backups"
chmod 711 "$root"
chmod 755 "$root/acme-webroot" "$root/acme-webroot/.well-known" "$root/acme-webroot/.well-known/acme-challenge"
if [[ -f $vhost ]]; then
    cp "$vhost" "$root/nginx-backups/$(date -u +%Y%m%dT%H%M%S)-$domain.conf"
fi
candidate=$root/nginx-candidate.conf
previous=$root/nginx-previous.conf
apply_config() {
    local had_previous=0
    if [[ -f $vhost ]]; then cp "$vhost" "$previous"; had_previous=1; fi
    install -m 644 "$candidate" "$vhost"
    if "$nginx" -t && "$nginx" -s reload; then return 0; fi
    if [[ $had_previous == 1 ]]; then cp "$previous" "$vhost"; else rm -f -- "$vhost"; fi
    "$nginx" -t && "$nginx" -s reload || true
    fail 'Nginx validation or reload failed; restored the previous virtual host.'
}
write_config() {
    local https=$1
    cat > "$candidate" <<NGINX
$marker
map \$http_upgrade \$$connection_var {
    default upgrade;
    '' close;
}
server {
    listen 80;
    server_name $domain;
    location ^~ /.well-known/acme-challenge/ {
        root $root/acme-webroot;
        default_type text/plain;
    }
NGINX
    if [[ $https == 1 ]]; then
        cat >> "$candidate" <<NGINX
    location / { return 301 https://$domain\$request_uri; }
}
server {
    listen 443 ssl;
    server_name $domain;
    ssl_certificate $root/tls/live/$domain/fullchain.pem;
    ssl_certificate_key $root/tls/live/$domain/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;
NGINX
    fi
    cat >> "$candidate" <<NGINX
    client_max_body_size 0;
    location / {
        proxy_pass http://127.0.0.1:$port;
        proxy_http_version 1.1;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
        proxy_set_header Upgrade \$http_upgrade;
        proxy_set_header Connection \$$connection_var;
        proxy_buffering off;
        proxy_request_buffering off;
        proxy_cache off;
    }
}
NGINX
}

certificate=$root/tls/live/$domain/fullchain.pem
if [[ ! -f $certificate ]]; then
    write_config 0
    apply_config
    printf 'Issuing a certificate for %s using an HTTP webroot challenge.\n' "$domain"
    docker run --rm -v "$root/tls:/etc/letsencrypt" -v "$root/acme-webroot:/var/www/acme" \
        certbot/certbot certonly --non-interactive --agree-tos --register-unsafely-without-email \
        --webroot -w /var/www/acme --cert-name "$domain" -d "$domain"
fi
openssl x509 -in "$certificate" -noout -checkend 0
openssl x509 -in "$certificate" -noout -checkhost "$domain"
write_config 1
apply_config
# A host hook reloads Nginx only after Certbot actually renews a certificate.
cat > "$root/renew-tls.sh" <<RENEW
#!/usr/bin/env bash
set -euo pipefail
exec 9>"$root/.tls-renew.lock"
flock -n 9 || exit 0
docker run --rm -v "$root/tls:/etc/letsencrypt" -v "$root/acme-webroot:/var/www/acme" \\
    certbot/certbot renew --quiet --deploy-hook 'touch /var/www/acme/.renewed'
if [[ -f $root/acme-webroot/.renewed ]]; then
    $nginx -t
    $nginx -s reload
    rm -f -- "$root/acme-webroot/.renewed"
fi
RENEW
chmod 700 "$root/renew-tls.sh"
touch "$root/tls-renew.log"
chmod 600 "$root/tls-renew.log"
printf 'PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n23 3,15 * * * root bash %s/renew-tls.sh >> %s/tls-renew.log 2>&1\n' \
    "$root" "$root" > "/etc/cron.d/$project-tls"
chmod 644 "/etc/cron.d/$project-tls"
printf 'HTTPS ready: https://%s/management.html\n' "$domain"
