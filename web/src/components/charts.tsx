import {
  useEffect,
  useRef,
  useState,
  type KeyboardEvent,
  type PointerEvent,
  type ReactNode,
} from "react";
import { useTranslation } from "react-i18next";

import { formatAxisTime } from "../lib/format";

export type ChartSeries = {
  key: string;
  label: string;
  /** The colour of the marks. Text never wears it: a stroke beside the text carries the identity. */
  color: string;
  values: number[];
  format: (value: number) => string;
  area?: boolean;
};

const chartHeight = 196;
const margin = { top: 8, right: 10, bottom: 24, left: 44 };

/** A hole in the data is drawn as a hole: a gap of more than this many samples breaks the line. */
const gapSamples = 2.5;

type Segment = { from: number; to: number };

/** The runs of consecutive samples, so that a line is not drawn across a gap. */
export function chartSegments(times: number[], resolution: number): Segment[] {
  const segments: Segment[] = [];
  let start = 0;

  for (let index = 1; index <= times.length; index++) {
    if (index === times.length || times[index] - times[index - 1] > resolution * gapSamples) {
      segments.push({ from: start, to: index - 1 });
      start = index;
    }
  }

  return times.length ? segments : [];
}

const minute = 60;
const hour = 60 * minute;
const day = 24 * hour;

/** Steps that give round times on an axis, smallest first. */
const tickSteps = [
  5 * minute,
  15 * minute,
  30 * minute,
  hour,
  3 * hour,
  6 * hour,
  12 * hour,
  day,
  2 * day,
  7 * day,
];

/**
 * Round times between `from` and `to` (in seconds) for the labels of a time axis:
 * whole quarters, hours or days in the browser's time zone, at most `most` of them.
 * The end of the range is "now", which has a label of its own, so a tick too close
 * to it is left out.
 */
export function timeTicks(
  from: number,
  to: number,
  most: number,
): { step: number; ticks: number[] } {
  const span = to - from;
  if (span <= 0 || most < 1) {
    return { step: 0, ticks: [] };
  }

  const step =
    tickSteps.find((candidate) => span / candidate <= most) ?? tickSteps[tickSteps.length - 1];
  const perDay = Math.max(1, Math.round(day / step));
  const daysPerStep = Math.max(1, Math.round(step / day));

  const ticks: number[] = [];
  const start = new Date(from * 1000);
  start.setHours(0, 0, 0, 0);

  // local dates, so that a change of the clock keeps the ticks on whole hours
  for (
    let date = start;
    date.getTime() / 1000 <= to;
    date = new Date(date.getFullYear(), date.getMonth(), date.getDate() + daysPerStep)
  ) {
    for (let index = 0; index < perDay; index++) {
      const time =
        new Date(
          date.getFullYear(),
          date.getMonth(),
          date.getDate(),
          0,
          (index * step) / minute,
        ).getTime() / 1000;
      if (time > from && time <= to - step / 2) {
        ticks.push(time);
      }
    }
  }

  return { step, ticks };
}

/** The index of the sample nearest to a time. */
export function nearestIndex(times: number[], time: number): number {
  let low = 0;
  let high = times.length - 1;

  while (high - low > 1) {
    const middle = (low + high) >> 1;
    if (times[middle] < time) {
      low = middle;
    } else {
      high = middle;
    }
  }

  return Math.abs(times[low] - time) <= Math.abs(times[high] - time) ? low : high;
}

function useWidth() {
  const element = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(640);

  useEffect(() => {
    const target = element.current;
    if (!target || typeof ResizeObserver === "undefined") {
      return;
    }

    const observer = new ResizeObserver(([entry]) => {
      if (entry.contentRect.width > 0) {
        setWidth(entry.contentRect.width);
      }
    });
    observer.observe(target);

    return () => observer.disconnect();
  }, []);

  return { element, width };
}

/**
 * A time chart with one vertical axis. Hover or the arrow keys read the values of
 * every series at one moment, and the table below it holds the same numbers.
 */
export function LineChart({
  label,
  times,
  from,
  to,
  resolution,
  series,
  maxValue,
  ticks,
  formatTick,
  formatTime,
  empty,
}: {
  label: string;
  times: number[];
  from: number;
  to: number;
  resolution: number;
  series: ChartSeries[];
  maxValue: number;
  ticks: number[];
  formatTick: (value: number) => string;
  formatTime: (time: number) => string;
  empty?: ReactNode;
}) {
  const { t } = useTranslation();
  const { element, width } = useWidth();
  const [cursor, setCursor] = useState<number | null>(null);

  const plotWidth = Math.max(width - margin.left - margin.right, 10);
  const plotHeight = chartHeight - margin.top - margin.bottom;

  const x = (time: number) =>
    margin.left + (Math.max(0, time - from) / Math.max(1, to - from)) * plotWidth;
  const y = (value: number) => margin.top + (1 - Math.min(value, maxValue) / maxValue) * plotHeight;

  const segments = chartSegments(times, resolution);

  const path = (values: number[]) =>
    segments
      .map(({ from: start, to: end }) => {
        let line = `M${x(times[start]).toFixed(1)} ${y(values[start]).toFixed(1)}`;
        for (let index = start + 1; index <= end; index++) {
          line += `L${x(times[index]).toFixed(1)} ${y(values[index]).toFixed(1)}`;
        }
        return line;
      })
      .join("");

  const area = (values: number[]) =>
    segments
      .filter(({ from: start, to: end }) => end > start)
      .map(({ from: start, to: end }) => {
        const base = y(0).toFixed(1);
        let shape = `M${x(times[start]).toFixed(1)} ${base}`;
        for (let index = start; index <= end; index++) {
          shape += `L${x(times[index]).toFixed(1)} ${y(values[index]).toFixed(1)}`;
        }
        return `${shape}L${x(times[end]).toFixed(1)} ${base}Z`;
      })
      .join("");

  // a label needs about 90 pixels, and more than seven make the axis hard to read
  const axis = timeTicks(from, to, Math.min(7, Math.max(2, Math.floor(plotWidth / 90))));

  const onPointerMove = (event: PointerEvent<SVGSVGElement>) => {
    if (!times.length) {
      return;
    }
    const bounds = event.currentTarget.getBoundingClientRect();
    const position = event.clientX - bounds.left - margin.left;
    setCursor(nearestIndex(times, from + (position / plotWidth) * (to - from)));
  };

  const onKeyDown = (event: KeyboardEvent<SVGSVGElement>) => {
    if (!times.length) {
      return;
    }
    if (event.key === "ArrowLeft" || event.key === "ArrowRight") {
      event.preventDefault();
      const step = event.key === "ArrowLeft" ? -1 : 1;
      setCursor((current) =>
        Math.min(times.length - 1, Math.max(0, (current ?? times.length) + step)),
      );
    } else if (event.key === "Escape") {
      setCursor(null);
    }
  };

  const active = cursor !== null && cursor < times.length ? cursor : null;
  const tooltipLeft = active === null ? 0 : x(times[active]);

  return (
    <div className="chart-frame" ref={element}>
      <svg
        className="chart"
        width={width}
        height={chartHeight}
        role="img"
        aria-label={label}
        tabIndex={0}
        onPointerMove={onPointerMove}
        onPointerLeave={() => setCursor(null)}
        onKeyDown={onKeyDown}
        onBlur={() => setCursor(null)}
      >
        {ticks.map((tick) => (
          <g key={tick}>
            <line
              className="chart-grid"
              x1={margin.left}
              x2={width - margin.right}
              y1={y(tick)}
              y2={y(tick)}
            />
            <text className="chart-axis" x={margin.left - 8} y={y(tick) + 4} textAnchor="end">
              {formatTick(tick)}
            </text>
          </g>
        ))}

        {axis.ticks.map((time) => (
          <text
            key={time}
            className="chart-axis"
            x={x(time)}
            y={chartHeight - 6}
            textAnchor="middle"
          >
            {formatAxisTime(time, axis.step)}
          </text>
        ))}
        <text className="chart-axis" x={x(to)} y={chartHeight - 6} textAnchor="end">
          {t("chart.now")}
        </text>

        {series.map((item) => (
          <g key={item.key}>
            {item.area && <path d={area(item.values)} fill={item.color} opacity={0.1} />}
            <path
              d={path(item.values)}
              fill="none"
              stroke={item.color}
              strokeWidth={2}
              strokeLinejoin="round"
              strokeLinecap="round"
            />
            {/* one sample between two gaps has no line to be seen on */}
            {segments
              .filter((segment) => segment.from === segment.to)
              .map((segment) => (
                <circle
                  key={segment.from}
                  cx={x(times[segment.from])}
                  cy={y(item.values[segment.from])}
                  r={2}
                  fill={item.color}
                />
              ))}
          </g>
        ))}

        {active !== null && (
          <g>
            <line
              className="chart-crosshair"
              x1={x(times[active])}
              x2={x(times[active])}
              y1={margin.top}
              y2={margin.top + plotHeight}
            />
            {series.map((item) => (
              <circle
                key={item.key}
                className="chart-dot"
                cx={x(times[active])}
                cy={y(item.values[active])}
                r={4}
                fill={item.color}
              />
            ))}
          </g>
        )}
      </svg>

      {times.length === 0 && empty && <div className="chart-empty">{empty}</div>}

      {active !== null && (
        <div
          className="chart-tooltip"
          style={
            tooltipLeft > width / 2
              ? { right: width - tooltipLeft + 12 }
              : { left: tooltipLeft + 12 }
          }
        >
          <div className="chart-tooltip-time">{formatTime(times[active])}</div>
          {series.map((item) => (
            <div key={item.key} className="chart-tooltip-row">
              <i style={{ background: item.color }} aria-hidden="true" />
              <strong>{item.format(item.values[active])}</strong>
              <span>{item.label}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

/** The same numbers as a chart, for reading them without a pointer. */
export function ChartTable({
  caption,
  times,
  series,
  formatTime,
}: {
  caption: string;
  times: number[];
  series: ChartSeries[];
  formatTime: (time: number) => string;
}) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);

  return (
    <details
      className="chart-table"
      onToggle={(event) => setOpen((event.target as HTMLDetailsElement).open)}
    >
      <summary>{t("chart.showTable")}</summary>
      {open && (
        <div className="chart-table-scroll" tabIndex={0} role="region" aria-label={caption}>
          <table className="table compact">
            <thead>
              <tr>
                <th>{t("chart.time")}</th>
                {series.map((item) => (
                  <th key={item.key}>{item.label}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {times
                .map((time, index) => ({ time, index }))
                .reverse()
                .map(({ time, index }) => (
                  <tr key={time}>
                    <td className="mono small">{formatTime(time)}</td>
                    {series.map((item) => (
                      <td key={item.key} className="num">
                        {item.format(item.values[index])}
                      </td>
                    ))}
                  </tr>
                ))}
            </tbody>
          </table>
        </div>
      )}
    </details>
  );
}
