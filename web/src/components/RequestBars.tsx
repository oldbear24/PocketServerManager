import { useState, type KeyboardEvent } from "react";
import { useTranslation } from "react-i18next";

import { formatHour, formatNumber, parseDate } from "../lib/format";

const barWidth = 8;
const gap = 2;
const height = 36;
const radius = 2;

/** A bar of the chart: rounded at its top, anchored to the baseline. */
function bar(x: number, h: number): string {
  const top = height - h;
  const r = Math.min(radius, h, barWidth / 2);
  return `M${x},${height}V${top + r}Q${x},${top} ${x + r},${top}H${x + barWidth - r}Q${x + barWidth},${top} ${x + barWidth},${top + r}V${height}Z`;
}

/**
 * The requests of a site in each of the last hours, as small bars in one
 * colour. Hovering a bar, or focusing the chart and moving with the arrow
 * keys, says its hour and count below it; a table holds the same numbers for
 * screen readers.
 */
export function RequestBars({ hours, from }: { hours: number[]; from: string }) {
  const { t } = useTranslation();
  const [active, setActive] = useState<number | null>(null);
  const start = parseDate(from)?.getTime() ?? 0;
  const max = Math.max(1, ...hours);
  const width = hours.length * (barWidth + gap) - gap;
  const hourOf = (index: number) => new Date(start + index * 3600_000);
  const describe = (index: number) =>
    t("site.requestsAt", {
      hour: formatHour(hourOf(index)),
      count: hours[index],
      formatted: formatNumber(hours[index]),
    });

  const move = (event: KeyboardEvent<HTMLDivElement>) => {
    const last = hours.length - 1;
    const current = active ?? last;
    const next =
      event.key === "ArrowLeft"
        ? Math.max(0, current - 1)
        : event.key === "ArrowRight"
          ? Math.min(last, current + 1)
          : event.key === "Home"
            ? 0
            : event.key === "End"
              ? last
              : null;
    if (next !== null) {
      event.preventDefault();
      setActive(next);
    }
  };

  return (
    <div className="request-bars">
      <div
        className="request-bars-plot"
        tabIndex={0}
        aria-label={t("site.requestsKeys")}
        onKeyDown={move}
        onFocus={() => setActive((current) => current ?? hours.length - 1)}
        onBlur={() => setActive(null)}
      >
        <svg
          viewBox={`0 0 ${width} ${height}`}
          preserveAspectRatio="none"
          aria-hidden="true"
          onMouseLeave={() => setActive(null)}
        >
          {hours.map((count, index) => {
            const x = index * (barWidth + gap);
            const h = count > 0 ? Math.max(2, (count / max) * height) : 0;
            return (
              <g key={index} onMouseEnter={() => setActive(index)}>
                {/* the whole column answers the pointer, not only the bar */}
                <rect
                  x={x - gap / 2}
                  y={0}
                  width={barWidth + gap}
                  height={height}
                  fill="transparent"
                />
                {h > 0 && (
                  <path
                    d={bar(x, h)}
                    fill="var(--chart-1)"
                    opacity={active === null || active === index ? 1 : 0.55}
                  />
                )}
              </g>
            );
          })}
        </svg>
      </div>
      <p className="request-bars-tip small muted" aria-hidden="true">
        {active !== null ? describe(active) : " "}
      </p>
      <table className="visually-hidden">
        <caption>{t("site.requestsTable")}</caption>
        <thead>
          <tr>
            <th scope="col">{t("site.hour")}</th>
            <th scope="col">{t("site.requests")}</th>
          </tr>
        </thead>
        <tbody>
          {hours.map((count, index) => (
            <tr key={index}>
              <td>{formatHour(hourOf(index))}</td>
              <td>{formatNumber(count)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
