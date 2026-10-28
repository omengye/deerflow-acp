# waku-client

`waku-client` is the Rust client for `waku-daemon`. It owns the authenticated
WebSocket handshake, request correlation, subscriptions, event sequence
deduplication, replay cursors, local-daemon supervision, and disposable client
preview caches. It depends on `waku-protocol`, never on `waku-core`.

Protocol 10 subscriptions fetch replay pages before delivering buffered live
transcript events. Only successfully delivered events and validated page
boundaries advance the resume cursor. Failed replay disconnects without
discarding the confirmed cursor. Live-only terminal events are delivered
immediately and never advance that cursor.

Both bare socket addresses and complete `ws://` or `wss://` URLs are accepted.
Dropping a connection to an externally managed daemon does not stop it.
