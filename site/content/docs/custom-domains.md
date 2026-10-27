---
title: Custom domains
description: Serve a quick site on your own hostname with a CNAME record.
---

# Custom domains

```bash
quick domains add launch www.example.com   # prints the DNS records to create
quick domains launch                       # active, or still waiting for DNS
quick domains rm launch www.example.com
```

At the domain's DNS provider, whichever it is, create:

```
www.example.com  CNAME  launch.quick.acme.com
```

Keep it DNS only, without a CDN proxy in front. An apex domain such as `example.com` can't have a CNAME: use your provider's ALIAS or CNAME flattening, or the same A record as the quick server, and add a TXT record with the site name so quick knows which site the domain is for:

```
_quick.example.com  TXT  launch
```

- The domain stays "waiting for DNS" until the record resolves through public resolvers (1.1.1.1, 8.8.8.8), then turns active on its own.
- The HTTPS certificate is issued on the first visit.
- A domain belongs to one site. It moves to another site only once its DNS points there (for an apex domain: once the TXT record names the other site).
- Search engines can index a site on its custom domain: the `noindex` quick puts on `<site>.<your-domain>` is not added there. Use `_headers` if you want it anyway.
- Public and access-code sites are served on the custom domain. Sites behind company sign-in redirect visitors to `<site>.<your-domain>`, because a sign-in does not carry across domains.

Custom domains have to be switched on by whoever runs the server: see [Self-hosting](/docs/self-hosting#custom-domains).
