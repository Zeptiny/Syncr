import "uplot/dist/uPlot.min.css";

import { useEffect, useRef } from "react";
import uPlot from "uplot";

/**
 * useUPlot mounts a uPlot chart in the returned element. The chart is
 * rebuilt when `deps` change (options, theme), its data is replaced in place
 * when `data` changes, and it follows the element's width.
 */
export function useUPlot(
  options: (width: number) => uPlot.Options | undefined,
  data: uPlot.AlignedData,
  deps: unknown[],
) {
  const box = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  const dataRef = useRef(data);

  useEffect(() => {
    dataRef.current = data;
    plot.current?.setData(data);
  }, [data]);

  useEffect(() => {
    const el = box.current;
    if (!el) return;
    const opts = options(el.clientWidth || 640);
    if (!opts) return;
    const u = new uPlot(opts, dataRef.current, el);
    plot.current = u;
    const ro = new ResizeObserver(() => {
      if (el.clientWidth > 0) u.setSize({ width: el.clientWidth, height: opts.height });
    });
    ro.observe(el);
    return () => {
      ro.disconnect();
      u.destroy();
      plot.current = null;
    };
    // The caller lists what the options depend on.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  return box;
}
