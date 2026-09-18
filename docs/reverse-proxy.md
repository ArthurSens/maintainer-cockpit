# Reverse proxy and HTTPS

Maintainer Cockpit serves HTTP and expects an administrator-provided TLS
reverse proxy. Bind the application to a private interface and expose only the
proxy. Set `external_base_url` to the exact public HTTPS origin so OAuth
callbacks, Secure cookies, and browser-origin checks agree.

## Caddy

With the application listening on `127.0.0.1:8765`:

```caddyfile
cockpit.example.org {
	reverse_proxy 127.0.0.1:8765
}
```

## nginx

```nginx
server {
    listen 443 ssl;
    server_name cockpit.example.org;

    # Configure certificate and key paths for this host.
    ssl_certificate /etc/letsencrypt/live/cockpit.example.org/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/cockpit.example.org/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8765;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
    }
}
```

Terminate HTTP or redirect it to HTTPS at the proxy. Do not publish the
application port on an internet-facing interface. Preserve the application's
security response headers rather than replacing them with weaker values.

After deployment, verify:

```sh
curl --fail https://cockpit.example.org/-/healthy
curl --fail https://cockpit.example.org/-/ready
```

Both endpoints return only `{"status":"ok"}` when healthy. Then run
`setup-check` with the production configuration and confirm the GitHub App
callback is exactly `https://cockpit.example.org/auth/callback`.
