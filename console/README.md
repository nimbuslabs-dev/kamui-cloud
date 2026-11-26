# Kamui Cloud Console

The Console is the Next.js dashboard for the Mobile Workspace MVP. It uses the Go workspace API for lifecycle and Git controls. nginx serves the Console over HTTPS and checks its session before forwarding IDE requests, including WebSocket upgrades.

Use the [Mobile Workspace guide](../docs/MOBILE_WORKSPACE_MVP.md) for the complete stack. Node.js 22 and npm are enough to build and test the Console in isolation.

```sh
npm ci --ignore-scripts
npm run lint
npm run type-check
npm run build
npm run test:auth
```

The auth regression test starts the production Console and a fixture API on ephemeral loopback ports. It covers login, logout, expiry, rate limiting, protected routes, CSRF, IDE origin checks and upstream response handling. It leaves Docker, code-server and GitHub testing to the stack acceptance tests.

The Console needs a single instance. Sessions last eight hours, and signing out or restarting the Console invalidates future requests using the affected session. An IDE WebSocket established before sign-out can remain connected until it closes or the workspace stops.
