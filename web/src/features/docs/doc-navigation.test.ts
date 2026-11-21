import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { keepInView } from "./doc-navigation";

/** A ResizeObserver whose callback the test calls (a reflow). */
class FakeObserver {
  static last: FakeObserver | undefined;
  observed: Element[] = [];
  disconnected = false;
  constructor(readonly callback: () => void) {
    FakeObserver.last = this;
  }
  observe(el: Element) {
    this.observed.push(el);
  }
  unobserve() {}
  disconnect() {
    this.disconnected = true;
  }
  reflow() {
    if (!this.disconnected) this.callback();
  }
}

let top = 81;
let section: HTMLElement;
let scroll: ReturnType<typeof vi.fn<(arg?: boolean | ScrollIntoViewOptions) => void>>;

beforeEach(() => {
  vi.useFakeTimers();
  vi.stubGlobal("ResizeObserver", FakeObserver);
  document.body.innerHTML = '<article><h2 id="conflicts">Conflicts</h2></article>';
  section = document.getElementById("conflicts")!;
  top = 81;
  section.getBoundingClientRect = () => ({ top }) as DOMRect;
  scroll = vi.fn<(arg?: boolean | ScrollIntoViewOptions) => void>(() => {
    top = 81;
  });
  section.scrollIntoView = scroll;
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("keepInView", () => {
  it("scrolls the section back when a reflow moved it", () => {
    keepInView(section);
    const observer = FakeObserver.last!;
    expect(observer.observed).toContain(document.querySelector("article"));
    observer.reflow();
    expect(scroll).not.toHaveBeenCalled();
    // The web font arrives: the text above the section gets shorter.
    top = -745;
    observer.reflow();
    expect(scroll).toHaveBeenCalledExactlyOnceWith({ block: "start" });
  });

  it("never pulls back a reader who scrolled", () => {
    keepInView(section);
    const observer = FakeObserver.last!;
    window.dispatchEvent(new WheelEvent("wheel", { deltaY: 200 }));
    expect(observer.disconnected).toBe(true);
    top = 400;
    observer.reflow();
    expect(scroll).not.toHaveBeenCalled();
  });

  it("stops on keys, touches and pointers too", () => {
    for (const event of [
      new KeyboardEvent("keydown", { key: "PageDown" }),
      new Event("touchstart"),
      new Event("pointerdown"),
    ]) {
      keepInView(section);
      window.dispatchEvent(event);
      expect(FakeObserver.last!.disconnected).toBe(true);
    }
  });

  it("aligns once more and stops when the page has settled", async () => {
    keepInView(section);
    const observer = FakeObserver.last!;
    top = 300;
    await vi.advanceTimersByTimeAsync(999);
    expect(observer.disconnected).toBe(false);
    await vi.advanceTimersByTimeAsync(100);
    expect(scroll).toHaveBeenCalledOnce();
    expect(observer.disconnected).toBe(true);
  });

  it("stops when the page that called it goes away", () => {
    const stop = keepInView(section);
    stop();
    expect(FakeObserver.last!.disconnected).toBe(true);
  });
});
