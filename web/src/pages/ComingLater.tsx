import { Construction } from "lucide-react";
import { useTranslation } from "react-i18next";

import { PageHead } from "../components/Layout";
import { Card, EmptyState } from "../components/ui";

const roadmap = "https://github.com/oldbear24/PocketServerManager/issues/40";

/** The sidebar shows the whole product; this says plainly what is not built yet. */
export function ComingLater({ feature, milestone }: { feature: string; milestone: string }) {
  const { t } = useTranslation();

  return (
    <>
      <PageHead title={t(`nav.${feature}`)} />
      <Card>
        <EmptyState
          icon={Construction}
          title={t("later.title", { milestone })}
          action={
            <a href={roadmap} target="_blank" rel="noreferrer">
              {t("later.roadmap")}
            </a>
          }
        >
          {t(`later.${feature}`)}
        </EmptyState>
      </Card>
    </>
  );
}
