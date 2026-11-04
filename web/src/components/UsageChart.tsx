import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { formatChartTime, formatNumber, percentOf } from "../lib/format";
import { metricRanges, type MetricPoints, type MetricRange } from "../lib/metrics";
import { ChartTable, LineChart, type ChartSeries } from "./charts";
import { Card, SegmentedControl } from "./ui";

/**
 * CPU and memory over time, on one axis in percent. A host's CPU is a share of
 * all its cores; a container's is on the scale of `docker stats`, where 100 % is
 * one core, so the axis grows past 100 % in steps of a whole core when a
 * container uses more. Memory is a share of what is available to whatever the
 * chart is about.
 */
export function UsageChart({
  title,
  points,
  stale,
  range,
  onRange,
  currentCpu,
  currentMem,
  footer,
}: {
  title: string;
  points: MetricPoints | null;
  stale: boolean;
  range: MetricRange;
  onRange: (range: MetricRange) => void;
  currentCpu: number;
  currentMem: ReactNode;
  footer?: ReactNode;
}) {
  const { t } = useTranslation();

  const times = points?.t ?? [];
  const memPercent = (points?.mem ?? []).map((value, index) =>
    percentOf(value, points?.mem_total[index] ?? 0),
  );

  const series: ChartSeries[] = [
    {
      key: "cpu",
      label: "CPU",
      color: "var(--chart-1)",
      values: points?.cpu ?? [],
      format: (value) => `${formatNumber(value, 1)} %`,
      area: true,
    },
    {
      key: "mem",
      label: "RAM",
      color: "var(--chart-2)",
      values: memPercent,
      format: (value) => `${formatNumber(value, 1)} %`,
    },
  ];

  const formatTime = (time: number) => formatChartTime(time, range);

  const maxValue = cpuAxisMax([...(points?.cpu ?? []), currentCpu]);

  return (
    <Card
      title={title}
      note={
        <span className="chart-legend" role="list">
          <span role="listitem">
            <i style={{ background: "var(--chart-1)" }} aria-hidden="true" />
            CPU <strong className="num">{formatNumber(currentCpu)} %</strong>
          </span>
          <span role="listitem">
            <i style={{ background: "var(--chart-2)" }} aria-hidden="true" />
            RAM <strong className="num">{currentMem}</strong>
          </span>
        </span>
      }
      side={
        <SegmentedControl
          label={t("chart.range")}
          value={range}
          onChange={onRange}
          options={metricRanges.map((value) => ({ value, label: t(`chart.ranges.${value}`) }))}
        />
      }
    >
      <div style={{ opacity: stale ? 0.5 : 1 }}>
        <LineChart
          label={t("chart.usageLabel", { title, range: t(`chart.ranges.${range}`) })}
          times={times}
          from={points?.from ?? 0}
          to={points?.to ?? 1}
          resolution={points?.resolution_s ?? 10}
          series={series}
          maxValue={maxValue}
          ticks={[0, maxValue / 2, maxValue]}
          formatTick={(tick) => `${tick} %`}
          formatTime={formatTime}
          empty={stale ? undefined : t("chart.noData")}
        />
      </div>
      {footer}
      <ChartTable caption={title} times={times} series={series} formatTime={formatTime} />
    </Card>
  );
}

/** cpuAxisMax is 100 %, or the next whole number of cores above the highest value. */
export function cpuAxisMax(values: number[]): number {
  const highest = Math.max(0, ...values.filter((value) => Number.isFinite(value)));
  return Math.max(100, Math.ceil(highest / 100) * 100);
}
