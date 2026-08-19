"use client";

/**
 * Resolve run rows' definition_ids to human labels (ticket 18.1). Fetches
 * `GET /v1/definitions/{id}` once per distinct id (through the proxy) and
 * caches the {name, version}; a run submitted from an inline definition has no
 * id and is labeled elsewhere.
 */
import { useEffect, useRef, useState } from "react";
import type { RunView } from "@agentloom/api-client";
import { browserApi } from "@/lib/api/browser";

export interface DefinitionLabel {
  name: string;
  version: number;
}

export function useDefinitionLabels(rows: RunView[]): Record<string, DefinitionLabel> {
  const [labels, setLabels] = useState<Record<string, DefinitionLabel>>({});
  const inFlight = useRef(new Set<string>());

  useEffect(() => {
    const ids = new Set<string>();
    for (const r of rows) if (r.definition_id) ids.add(r.definition_id);
    for (const id of ids) {
      if (labels[id] || inFlight.current.has(id)) continue;
      inFlight.current.add(id);
      void browserApi()
        .GET("/v1/definitions/{definition_id}", { params: { path: { definition_id: id } } })
        .then(({ data }) => {
          if (data) setLabels((prev) => ({ ...prev, [id]: { name: data.name, version: data.version } }));
        })
        .finally(() => inFlight.current.delete(id));
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rows]);

  return labels;
}
