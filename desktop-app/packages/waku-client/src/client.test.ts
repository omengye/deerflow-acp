import { describe, expect, test } from "bun:test";

import {
  WakuClient,
  WakuConnectionError,
  WakuRpcError,
  daemonUrl,
  type WebSocketLike,
} from "./client";
import { PROTOCOL_VERSION, type SequencedEvent } from "./generated";

class FakeSocket implements WebSocketLike {
  readyState = 0;
  sent: string[] = [];
  private listeners = new Map<string, Array<(...args: any[]) => void>>();

  addEventListener(type: "open", listener: () => void): void;
  addEventListener(type: "message", listener: (event: MessageEvent) => void): void;
  addEventListener(type: "error", listener: () => void): void;
  addEventListener(type: "close", listener: (event: CloseEvent) => void): void;
  addEventListener(type: string, listener: (...args: any[]) => void): void {
    const listeners = this.listeners.get(type) ?? [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }

  send(data: string): void {
    this.sent.push(data);
  }

  close(): void {
    this.readyState = 3;
  }

  emitClose(): void {
    this.readyState = 3;
    this.emit("close", { reason: "" });
  }

  fail(reason: string): void {
    this.readyState = 3;
    this.emit("error", {});
    this.emit("close", { reason });
  }

  open(): void {
    this.readyState = 1;
    this.emit("open");
  }

  receive(message: unknown): void {
    this.emit("message", { data: JSON.stringify(message) });
  }

  private emit(type: string, event?: unknown): void {
    for (const listener of this.listeners.get(type) ?? []) listener(event);
  }
}

function fixture() {
  const sockets: FakeSocket[] = [];
  let nextId = 0;
  const client = new WakuClient({
    address: "127.0.0.1:4312",
    token: "secret",
    randomUUID: () => `00000000-0000-4000-8000-${String(++nextId).padStart(12, "0")}`,
    webSocketFactory: () => {
      const socket = new FakeSocket();
      sockets.push(socket);
      return socket;
    },
  });
  return { client, sockets };
}

async function connect(client: WakuClient, sockets: FakeSocket[]): Promise<FakeSocket> {
  const connected = client.connect();
  const socket = sockets.at(-1)!;
  socket.open();
  socket.receive({ type: "hello", protocolVersion: PROTOCOL_VERSION, daemonVersion: "test" });
  await connected;
  return socket;
}

function delta(sequence: number, epoch = "epoch"): SequencedEvent {
  return { sessionId: "session", runtimeId: "runtime", epoch, sequence, replayable: true,
    event: { kind: "textDelta", payload: `piece-${sequence}` } };
}

function lastReplay(socket: FakeSocket) {
  return socket.sent.map((entry) => JSON.parse(entry))
    .filter((entry) => entry.type === "request" && entry.command.type === "replayEvents").at(-1);
}

async function replayPage(socket: FakeSocket, events: SequencedEvent[], throughSequence: number,
  hasMore = false, epoch = events[0]?.epoch ?? "epoch") {
  const request = lastReplay(socket);
  expect(request).toBeDefined();
  socket.receive({ type: "response", requestId: request.requestId,
    outcome: { status: "ok", payload: { type: "replayEvents", epoch, events, throughSequence,
      nextSequence: hasMore ? events.at(-1)!.sequence : throughSequence, hasMore } } });
  await Promise.resolve();
  await Promise.resolve();
}

describe("WakuClient", () => {
  test("reports connection state changes, including remote closure", async () => {
    const { client, sockets } = fixture();
    const states: string[] = [];
    const unsubscribe = client.subscribeConnectionState((state) => states.push(state));

    const connected = client.connect();
    const socket = sockets[0]!;
    socket.open();
    socket.receive({ type: "hello", protocolVersion: PROTOCOL_VERSION, daemonVersion: "test" });
    await connected;
    socket.close();
    socket.emitClose();
    unsubscribe();

    expect(states).toEqual(["disconnected", "connecting", "connected", "disconnected"]);
  });

  test("authenticates and correlates typed responses", async () => {
    const { client, sockets } = fixture();
    const connected = client.connect();
    const socket = sockets[0]!;
    socket.open();
    expect(JSON.parse(socket.sent[0]!)).toEqual({
      type: "hello",
      protocolVersion: PROTOCOL_VERSION,
      token: "secret",
      clientId: "00000000-0000-4000-8000-000000000001",
      resumeFrom: [],
    });
    socket.receive({ type: "hello", protocolVersion: PROTOCOL_VERSION, daemonVersion: "test" });
    await connected;

    const response = client.request({ type: "getSettings" });
    const request = JSON.parse(socket.sent[1]!);
    socket.receive({
      type: "response",
      requestId: request.requestId,
      outcome: { status: "ok", payload: { type: "ack" } },
    });
    await expect(response).resolves.toEqual({ type: "ack" });
  });

  test("surfaces daemon errors", async () => {
    const { client, sockets } = fixture();
    const socket = sockets[0] ?? new FakeSocket();
    const connected = client.connect();
    const active = sockets[0] ?? socket;
    active.open();
    active.receive({ type: "hello", protocolVersion: PROTOCOL_VERSION, daemonVersion: "test" });
    await connected;

    const response = client.request({ type: "getSettings" });
    const request = JSON.parse(active.sent[1]!);
    active.receive({
      type: "response",
      requestId: request.requestId,
      outcome: { status: "error", error: { message: "nope" } },
    });
    await expect(response).rejects.toBeInstanceOf(WakuRpcError);
  });

  test("deduplicates events and resumes from the last sequence", async () => {
    const { client, sockets } = fixture();
    const firstConnection = client.connect();
    const first = sockets[0]!;
    first.open();
    first.receive({ type: "hello", protocolVersion: PROTOCOL_VERSION, daemonVersion: "test" });
    await firstConnection;

    const received: number[] = [];
    client.subscribe("session", "runtime", (event) => received.push(event.sequence));
    await replayPage(first, [], 0, false, "epoch-one");
    const event = {
      type: "event",
      sessionId: "session",
      runtimeId: "runtime",
      epoch: "epoch-one",
      sequence: 4,
      event: { kind: "textDelta", payload: { text: "hi" } },
    };
    first.receive(event);
    first.receive(event);
    expect(received).toEqual([4]);

    client.disconnect();
    const secondConnection = client.connect();
    const second = sockets[1]!;
    second.open();
    expect(JSON.parse(second.sent[0]!).resumeFrom).toEqual([
      { sessionId: "session", runtimeId: "runtime", epoch: "epoch-one", sequence: 4 },
    ]);
    second.receive({ type: "hello", protocolVersion: PROTOCOL_VERSION, daemonVersion: "test" });
    await secondConnection;
    expect(lastReplay(second).command.afterSequence).toBe(4);
    await replayPage(second, [], 4, false, "epoch-one");
    client.disconnect();
  });

  test("disconnect rejects an in-flight handshake and permits reconnecting", async () => {
    const { client, sockets } = fixture();
    const firstConnection = client.connect();
    client.disconnect();
    await expect(firstConnection).rejects.toThrow("Waku client disconnected");

    const secondConnection = client.connect();
    const second = sockets[1]!;
    second.open();
    second.receive({ type: "hello", protocolVersion: PROTOCOL_VERSION, daemonVersion: "test" });
    await expect(secondConnection).resolves.toBeUndefined();
  });

  test("surfaces the native socket failure reason", async () => {
    const { client, sockets } = fixture();
    const connection = client.connect();

    sockets[0]!.fail("The network connection was lost");

    await expect(connection).rejects.toThrow("The network connection was lost");
  });

  test("recovers from sequence one when reconnecting to a new daemon epoch", async () => {
    const { client, sockets } = fixture();
    const socket = await connect(client, sockets);
    const received: Array<[string, number]> = [];
    client.subscribe("session", "runtime", (event) => {
      received.push([event.epoch, event.sequence]);
    });

    await replayPage(socket, [delta(9, "old")], 9);
    client.disconnect();
    const replacement = await connect(client, sockets);
    expect(lastReplay(replacement).command).toMatchObject({ epoch: "old", afterSequence: 9 });
    await replayPage(replacement, [delta(1, "new")], 1);

    expect(received).toEqual([
      ["old", 9],
      ["new", 1],
    ]);
    client.disconnect();
  });

  test("does not advance unobserved runtimes and recovers them when an app attaches", async () => {
    const { client, sockets } = fixture();
    const socket = await connect(client, sockets);

    socket.receive({
      type: "event",
      sessionId: "session",
      runtimeId: "runtime",
      epoch: "epoch",
      sequence: 1,
      event: { kind: "textDelta", payload: "before attach" },
    });
    socket.receive({
      type: "event",
      sessionId: "session",
      runtimeId: "runtime",
      epoch: "epoch",
      sequence: 2,
      event: { kind: "textDelta", payload: "still before attach" },
    });

    const received: number[] = [];
    expect(client.replayCursors()).toEqual([]);
    client.subscribe("session", "runtime", (event) => received.push(event.sequence));
    expect(received).toEqual([]);
    await replayPage(socket, [delta(1), delta(2)], 2);
    expect(received).toEqual([1, 2]);
    client.disconnect();
  });

  test("notifies connected apps when another client changes task state", async () => {
    const { client, sockets } = fixture();
    const socket = await connect(client, sockets);
    const revisions: number[] = [];
    client.subscribeTaskState((revision) => revisions.push(revision));

    socket.receive({ type: "taskStateChanged", revision: 7 });
    expect(revisions).toEqual([7]);
  });

  test("disconnected requests reject instead of throwing synchronously", async () => {
    const { client } = fixture();
    const request = client.request({ type: "getSettings" });
    await expect(request).rejects.toThrow("Waku daemon is disconnected");
  });

  test("disconnected notifications reject instead of throwing synchronously", async () => {
    const { client } = fixture();
    const notification = client.notify({ type: "refreshBackgroundWork" });
    await expect(notification).rejects.toThrow("Waku daemon is disconnected");
  });

  test("notifications use the response-free nil request id", async () => {
    const { client, sockets } = fixture();
    const socket = await connect(client, sockets);

    await client.notify(
      { type: "writeTerminal", data: "bHM=" },
      "terminal",
      "terminal",
    );

    expect(JSON.parse(socket.sent[1]!)).toEqual({
      type: "request",
      requestId: "00000000-0000-0000-0000-000000000000",
      sessionId: "terminal",
      runtimeId: "terminal",
      command: { type: "writeTerminal", data: "bHM=" },
    });
  });
});

test("daemonUrl pins the versioned endpoint", () => {
  expect(daemonUrl("localhost:3030/anything?old=1")).toBe("ws://localhost:3030/v1");
  expect(daemonUrl("wss://waku.example.test")).toBe("wss://waku.example.test/v1");
});

describe("WakuClient complete event recovery", () => {
  test("recovers more than the old 2048-event window before buffered live events", async () => {
    const { client, sockets } = fixture();
    const socket = await connect(client, sockets);
    const received: number[] = [];
    client.subscribe("session", "runtime", (event) => received.push(event.sequence));
    socket.receive({ type: "event", ...delta(2050) });
    socket.receive({ type: "event", ...delta(2051) });
    expect(received).toEqual([]);
    expect(client.replayCursors()).toEqual([]);
    for (let after = 0; after < 2050;) {
      const next = Math.min(after + 256, 2050);
      expect(lastReplay(socket).command).toMatchObject({
        afterSequence: after,
        throughSequence: after === 0 ? null : 2050,
        limit: 256,
      });
      const events = Array.from({ length: next - after }, (_, index) => delta(after + index + 1));
      await replayPage(socket, events, 2050, next < 2050);
      after = next;
    }
    expect(received).toEqual(Array.from({ length: 2051 }, (_, index) => index + 1));
    expect(client.replayCursors()[0]?.sequence).toBe(2051);
    expect(client.connected).toBe(true);
    client.disconnect();
  });

  test("reconnect catches up existing subscriptions before resuming live delivery", async () => {
    const { client, sockets } = fixture();
    const first = await connect(client, sockets);
    const received: number[] = [];
    client.subscribe("session", "runtime", (event) => received.push(event.sequence));
    await replayPage(first, [delta(1)], 1);
    first.emitClose();
    const second = await connect(client, sockets);
    expect(lastReplay(second).command.afterSequence).toBe(1);
    second.receive({ type: "event", ...delta(3) });
    expect(received).toEqual([1]);
    await replayPage(second, [delta(2)], 2);
    expect(received).toEqual([1, 2, 3]);
    client.disconnect();
  });

  test("ephemeral holes use the server snapshot cursor without inventing missing events", async () => {
    const { client, sockets } = fixture();
    const socket = await connect(client, sockets);
    const received: number[] = [];
    client.subscribe("session", "runtime", (event) => received.push(event.sequence));
    await replayPage(socket, [delta(1), delta(3)], 5);
    expect(client.replayCursors()[0]?.sequence).toBe(5);
    socket.receive({ type: "event", ...delta(7) });
    expect(received).toEqual([1, 3, 7]);
    expect(client.connected).toBe(true);
    client.disconnect();
  });

  test("subscribing before opening a terminal delivers transient output during recovery without acknowledging it", async () => {
    const { client, sockets } = fixture();
    const socket = await connect(client, sockets);
    const received: string[] = [];
    client.subscribe("session", "runtime", (event) => received.push(event.event.kind));
    const opening = client.request({ type: "openTerminal", cwd: "/work", cols: 80, rows: 24 }, "session", "runtime");
    const openRequest = JSON.parse(socket.sent.at(-1)!);
    socket.receive({ type: "event", ...delta(1), replayable: false,
      event: { kind: "terminalOutput", payload: "first prompt" } });
    socket.receive({ type: "event", ...delta(2), replayable: false,
      event: { kind: "terminalExited", payload: 0 } });
    expect(received).toEqual(["terminalOutput", "terminalExited"]);
    expect(client.replayCursors()).toEqual([]);

    socket.receive({ type: "response", requestId: openRequest.requestId,
      outcome: { status: "ok", payload: { type: "ack" } } });
    await opening;
    await replayPage(socket, [], 2);
    // Even a sequence at/below the persistent snapshot is not a duplicate for
    // terminal output. Delivery must leave that persistent cursor unchanged.
    socket.receive({ type: "event", ...delta(1), replayable: false,
      event: { kind: "terminalOutput", payload: "more output" } });
    expect(received).toEqual(["terminalOutput", "terminalExited", "terminalOutput"]);
    expect(client.replayCursors()[0]?.sequence).toBe(2);
    expect(client.connected).toBe(true);
    client.disconnect();
  });

  test("a changed snapshot fails closed without committing buffered live events", async () => {
    const { client, sockets } = fixture();
    const socket = await connect(client, sockets);
    const received: number[] = [];
    client.subscribe("session", "runtime", (event) => received.push(event.sequence));
    await replayPage(socket, [delta(1)], 3, true);
    socket.receive({ type: "event", ...delta(4) });
    await replayPage(socket, [delta(2)], 4, true);
    expect(client.connected).toBe(false);
    expect(received).toEqual([1]);
    expect(client.replayCursors()[0]?.sequence).toBe(1);
    expect(client.lastDisconnectReason).toContain("snapshot changed");
  });

  test("a replay error preserves the confirmed cursor and requests it after reconnect", async () => {
    const { client, sockets } = fixture();
    const first = await connect(client, sockets);
    const received: number[] = [];
    client.subscribe("session", "runtime", (event) => received.push(event.sequence));
    await replayPage(first, [delta(1)], 3, true);
    first.receive({ type: "event", ...delta(4) });
    first.receive({ type: "response", requestId: lastReplay(first).requestId,
      outcome: { status: "error", error: { message: "journal unavailable" } } });
    await Promise.resolve();
    await Promise.resolve();
    expect(client.connected).toBe(false);
    expect(received).toEqual([1]);
    const second = await connect(client, sockets);
    expect(lastReplay(second).command.afterSequence).toBe(1);
    await replayPage(second, [delta(2), delta(3), delta(4)], 4);
    expect(received).toEqual([1, 2, 3, 4]);
    client.disconnect();
  });

  test("overflow disconnects instead of dropping the oldest live events or advancing cursors", async () => {
    const { client, sockets } = fixture();
    const socket = await connect(client, sockets);
    const received: number[] = [];
    client.subscribe("session", "runtime", (event) => received.push(event.sequence));
    for (let sequence = 1; sequence <= 4097; sequence++) socket.receive({ type: "event", ...delta(sequence) });
    expect(client.connected).toBe(false);
    expect(client.replayCursors()).toEqual([]);
    expect(received).toEqual([]);
    expect(client.lastDisconnectReason).toContain("overflow");
    await Promise.resolve();
  });

  test("validates a complete page before delivering foreign or unordered events", async () => {
    for (const bad of [
      { ...delta(2), runtimeId: "foreign" },
      { ...delta(2), replayable: false },
      delta(1),
    ]) {
      const { client, sockets } = fixture();
      const socket = await connect(client, sockets);
      const received: number[] = [];
      client.subscribe("session", "runtime", (event) => received.push(event.sequence));
      await replayPage(socket, [delta(1), bad], 2);
      expect(client.connected).toBe(false);
      expect(client.replayCursors()).toEqual([]);
      expect(received).toEqual([]);
    }
  });

  test("unsubscribing fences a late recovery response and later live events", async () => {
    const { client, sockets } = fixture();
    const socket = await connect(client, sockets);
    const received: number[] = [];
    const unsubscribe = client.subscribe("session", "runtime", (event) => received.push(event.sequence));
    unsubscribe();
    await replayPage(socket, [delta(1)], 1);
    socket.receive({ type: "event", ...delta(2) });
    expect(client.replayCursors()).toEqual([]);
    expect(received).toEqual([]);
    client.disconnect();
  });
});

describe("WakuClient connection failures", () => {
  async function failure(setup: (socket: FakeSocket) => void): Promise<WakuConnectionError> {
    const { client, sockets } = fixture();
    const connected = client.connect();
    setup(sockets[0]!);
    const error = await connected.catch((cause: unknown) => cause);
    expect(error).toBeInstanceOf(WakuConnectionError);
    return error as WakuConnectionError;
  }

  test("types handshake failures so callers can tell what needs a person", async () => {
    const rejected = await failure((socket) => {
      socket.open();
      socket.receive({ type: "rejected", message: "authentication failed" });
    });
    expect(rejected.kind).toBe("rejected");
    expect(rejected.retryable).toBe(false);
    expect(rejected.message).toBe("daemon rejected connection: authentication failed");

    const unsupported = await failure((socket) => {
      socket.open();
      socket.receive({ type: "rejected", message: "protocol 3 is unsupported; expected 7" });
    });
    expect(unsupported.kind).toBe("protocol");

    const mismatch = await failure((socket) => {
      socket.open();
      socket.receive({ type: "hello", protocolVersion: PROTOCOL_VERSION + 1, daemonVersion: "x" });
    });
    expect(mismatch.kind).toBe("protocol");
    expect(mismatch.retryable).toBe(false);

    const garbage = await failure((socket) => {
      socket.open();
      socket.receive({ type: "event", sessionId: "x" });
    });
    expect(garbage.kind).toBe("handshake");

    const refused = await failure((socket) => socket.fail("Connection refused"));
    expect(refused.kind).toBe("unreachable");
    expect(refused.retryable).toBe(true);
    expect(refused.message).toBe("Connection refused");
  });

  test("times out a handshake the daemon never answers", async () => {
    const sockets: FakeSocket[] = [];
    const client = new WakuClient({
      address: "127.0.0.1:4312",
      token: "secret",
      connectTimeoutMs: 1,
      randomUUID: () => "00000000-0000-4000-8000-000000000001",
      webSocketFactory: () => {
        const socket = new FakeSocket();
        sockets.push(socket);
        return socket;
      },
    });
    const error = (await client.connect().catch((cause: unknown) => cause)) as WakuConnectionError;
    expect(error.kind).toBe("timeout");
    expect(error.retryable).toBe(true);
    expect(client.connectionState).toBe("disconnected");
    expect(sockets[0]!.readyState).toBe(3);

    // The abandoned socket's late events are ignored and a fresh attempt works.
    sockets[0]!.open();
    const again = client.connect();
    expect(sockets).toHaveLength(2);
    sockets[1]!.open();
    sockets[1]!.receive({ type: "hello", protocolVersion: PROTOCOL_VERSION, daemonVersion: "test" });
    await again;
    expect(client.connected).toBe(true);
  });

  test("lets a request shorten its own timeout", async () => {
    const { client, sockets } = fixture();
    await connect(client, sockets);
    await expect(
      client.request({ type: "getSettings" }, undefined, undefined, { timeoutMs: 1 }),
    ).rejects.toThrow("timed out waiting for Waku daemon");
  });

  test("settles requests and clears the socket before listeners hear a remote close", async () => {
    const { client, sockets } = fixture();
    const socket = await connect(client, sockets);
    const request = client.request({ type: "getSettings" });
    let reconnected: Promise<void> | null = null;
    client.subscribeConnectionState((state) => {
      if (state === "disconnected" && !reconnected) reconnected = client.connect();
    });

    socket.fail("Software caused connection abort");
    const error = (await request.catch((cause: unknown) => cause)) as WakuConnectionError;
    expect(error.kind).toBe("closed");
    expect(client.lastDisconnectReason).toBe("Software caused connection abort");
    expect(sockets).toHaveLength(2);

    sockets[1]!.open();
    sockets[1]!.receive({ type: "hello", protocolVersion: PROTOCOL_VERSION, daemonVersion: "test" });
    await reconnected!;
    expect(client.connected).toBe(true);
    const next = client.request({ type: "getSettings" });
    expect(sockets[1]!.sent).toHaveLength(2);
    const sent = JSON.parse(sockets[1]!.sent[1]!) as { requestId: string };
    sockets[1]!.receive({
      type: "response",
      requestId: sent.requestId,
      outcome: { status: "ok", payload: { type: "ack" } },
    });
    await expect(next).resolves.toEqual({ type: "ack" });
  });

  test("records when the daemon last spoke", async () => {
    const sockets: FakeSocket[] = [];
    let now = 41;
    const client = new WakuClient({
      address: "127.0.0.1:4312",
      token: "secret",
      now: () => now,
      randomUUID: () => "00000000-0000-4000-8000-000000000001",
      webSocketFactory: () => {
        const socket = new FakeSocket();
        sockets.push(socket);
        return socket;
      },
    });
    expect(client.lastMessageAt).toBe(0);
    const connected = client.connect();
    now = 42;
    sockets[0]!.open();
    sockets[0]!.receive({ type: "hello", protocolVersion: PROTOCOL_VERSION, daemonVersion: "test" });
    await connected;
    expect(client.lastMessageAt).toBe(42);
    now = 43;
    sockets[0]!.receive({ type: "taskStateChanged", revision: 1 });
    expect(client.lastMessageAt).toBe(43);
  });
});
