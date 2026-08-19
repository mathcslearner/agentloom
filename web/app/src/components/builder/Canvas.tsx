"use client";

// Canvas (ticket 17.3) — the React Flow surface. Registers StepNode for every
// catalog step type and the two edge renderers, wires the store's change /
// connect handlers, drag-batches moves for undo, accepts palette drops at the
// cursor, and enforces the port rules on new connections. Minimap, zoom/fit
// controls, and a dotted background come from React Flow. Delete is handled by
// the keyboard hook (deleteKeyCode is disabled) so there is one deletion path.

import { useCallback, useMemo } from "react";
import {
  Background,
  Controls,
  MiniMap,
  ReactFlow,
  useReactFlow,
  type Connection,
  type Edge,
  type NodeTypes,
  type EdgeTypes,
} from "@xyflow/react";
import { STEP_TYPES } from "@agentloom/graphdef";
import { isValidConnection as portsValid } from "@/lib/pure/builder/ports";
import { beginInteraction, endInteraction, useBuilderStore } from "@/lib/builder/store";
import { StepNode } from "./StepNode";
import { LoopEdge, StepEdge } from "./edges";
import { STEP_DND_MIME } from "./Palette";

const nodeTypes: NodeTypes = Object.fromEntries(STEP_TYPES.map((t) => [t, StepNode])) as NodeTypes;
const edgeTypes: EdgeTypes = { step: StepEdge, loop: LoopEdge };

export function Canvas() {
  const nodes = useBuilderStore((s) => s.nodes);
  const edges = useBuilderStore((s) => s.edges);
  const onNodesChange = useBuilderStore((s) => s.onNodesChange);
  const onEdgesChange = useBuilderStore((s) => s.onEdgesChange);
  const connect = useBuilderStore((s) => s.connect);
  const addStep = useBuilderStore((s) => s.addStep);
  const { screenToFlowPosition } = useReactFlow();

  const isValid = useCallback((c: Connection | Edge) => portsValid(c, useBuilderStore.getState().edges), []);

  const onDrop = useCallback(
    (event: React.DragEvent) => {
      event.preventDefault();
      const type = event.dataTransfer.getData(STEP_DND_MIME);
      if (!type) return;
      const position = screenToFlowPosition({ x: event.clientX, y: event.clientY });
      addStep(type as never, position);
    },
    [addStep, screenToFlowPosition],
  );

  const onDragOver = useCallback((event: React.DragEvent) => {
    event.preventDefault();
    event.dataTransfer.dropEffect = "copy";
  }, []);

  const proOptions = useMemo(() => ({ hideAttribution: true }), []);

  return (
    <div className="h-full w-full" data-testid="builder-canvas">
      <ReactFlow
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        edgeTypes={edgeTypes}
        onNodesChange={onNodesChange}
        onEdgesChange={onEdgesChange}
        onConnect={(c) => connect(c)}
        isValidConnection={isValid}
        onNodeDragStart={beginInteraction}
        onNodeDragStop={endInteraction}
        onSelectionDragStart={beginInteraction}
        onSelectionDragStop={endInteraction}
        onDrop={onDrop}
        onDragOver={onDragOver}
        deleteKeyCode={null}
        multiSelectionKeyCode={["Meta", "Shift"]}
        selectionKeyCode="Shift"
        proOptions={proOptions}
        fitView
        fitViewOptions={{ padding: 0.2 }}
      >
        <Background />
        <MiniMap pannable zoomable />
        <Controls />
      </ReactFlow>
    </div>
  );
}
