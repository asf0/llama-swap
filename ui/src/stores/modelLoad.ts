import { writable } from "svelte/store";
import { loadModel, unloadSingleModel } from "./api";
import type { Model } from "../lib/types";

export const pendingLoads = writable<Record<string, boolean>>({});
const loadControllers = new Map<string, AbortController>();

export async function handleLoadModel(id: string): Promise<void> {
  if (isPending(id)) return;

  const controller = new AbortController();
  loadControllers.set(id, controller);
  pendingLoads.update((p) => ({ ...p, [id]: true }));
  try {
    await loadModel(id, controller.signal);
  } catch (e) {
    console.error(e);
  } finally {
    loadControllers.delete(id);
    pendingLoads.update((p) => {
      const next = { ...p };
      delete next[id];
      return next;
    });
  }
}

export function cancelLoad(id: string): void {
  loadControllers.get(id)?.abort();
}

export function isPending(id: string): boolean {
  let val = false;
  pendingLoads.subscribe((p) => (val = !!p[id]))();
  return val;
}

export function onToggleLoad(m: Model): void {
  // Peer state comes from the peer poller and starts as unknown. Treat that
  // like stopped so the user can still ask the peer to load the model.
  const unknownPeer = !!m.peerID && (!m.state || m.state === "unknown");
  const stopped = m.state === "stopped" || unknownPeer;
  if (stopped && isPending(m.id)) {
    cancelLoad(m.id);
  } else if (stopped) {
    void handleLoadModel(m.id);
  } else if (m.state === "ready") {
    void unloadSingleModel(m.id);
  }
}

// loadingPercent is the reported loading progress as a whole percentage, or
// undefined when the model is not starting or its upstream reports none. It
// rounds down so a load that is not finished never shows 100%.
export function loadingPercent(m: Model | undefined): number | undefined {
  if (m?.state !== "starting" || m.loadingProgress === undefined) return undefined;
  return Math.floor(Math.min(Math.max(m.loadingProgress, 0), 1) * 100);
}

// loadingMessage is the upstream's reported loading step while starting.
export function loadingMessage(m: Model | undefined): string {
  return m?.state === "starting" ? (m.loadingMessage ?? "") : "";
}

export function statusDotColor(m: Model | undefined): string {
  if (!m) return "bg-muted-foreground/40";
  if (m.state === "ready") return "bg-success";
  if (m.state === "starting" || m.state === "stopping") return "bg-warning";
  return "bg-muted-foreground/40";
}
