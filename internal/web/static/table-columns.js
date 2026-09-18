const definitions = [
  { id: "actions", label: "Actions", fixed: true, defaultVisible: true },
  { id: "number", label: "PR", fixed: true, defaultVisible: true },
  { id: "repository", label: "Repository", fixed: true, defaultVisible: true },
  { id: "title", label: "Title", fixed: true, defaultVisible: true },
  { id: "contribution", label: "Author Contribution History", defaultVisible: false },
  { id: "churn", label: "Size", defaultVisible: true },
  { id: "quality", label: "PR Quality", defaultVisible: true },
  { id: "review_load", label: "Review Cognitive Load", defaultVisible: true },
  { id: "waiting", label: "Waiting On", defaultVisible: true },
  { id: "review", label: "Review", defaultVisible: false },
  { id: "updated", label: "Updated", defaultVisible: true },
];

function availableDefinitions(showActions) {
  return definitions.filter(({ id }) => showActions || id !== "actions");
}

export function defaultTableLayout({ showActions = false } = {}) {
  return availableDefinitions(showActions).map((definition) => ({
    ...definition,
    visible: definition.defaultVisible,
  }));
}

export function normalizeTableLayout(saved, { showActions = false } = {}) {
  const defaults = defaultTableLayout({ showActions });
  if (!Array.isArray(saved)) {
    return defaults;
  }
  const byID = new Map(defaults.map((column) => [column.id, column]));
  const seen = new Set();
  const restored = [];
  for (const item of saved) {
    const column = byID.get(item?.id);
    if (!column || seen.has(column.id)) {
      continue;
    }
    seen.add(column.id);
    restored.push({
      ...column,
      visible: column.fixed ? true : item.visible !== false,
    });
  }
  restored.push(...defaults.filter(({ id }) => !seen.has(id)));
  if (showActions) {
    const actions = restored.find(({ id }) => id === "actions");
    return [actions, ...restored.filter(({ id }) => id !== "actions")];
  }
  return restored;
}

export function visibleTableColumns(layout) {
  return layout.filter(({ visible }) => visible);
}

export function toggleTableColumn(layout, id) {
  return layout.map((column) => column.id === id && !column.fixed
    ? { ...column, visible: !column.visible }
    : column);
}

export function reorderTableColumn(layout, sourceID, targetID) {
  if (sourceID === "actions" || sourceID === targetID) {
    return layout;
  }
  const sourceIndex = layout.findIndex(({ id }) => id === sourceID);
  const targetIndex = layout.findIndex(({ id }) => id === targetID);
  if (sourceIndex < 0 || targetIndex < 0) {
    return layout;
  }
  const reordered = [...layout];
  const [source] = reordered.splice(sourceIndex, 1);
  let insertionIndex = reordered.findIndex(({ id }) => id === targetID);
  if (sourceIndex < targetIndex) {
    insertionIndex++;
  }
  reordered.splice(Math.max(insertionIndex, reordered[0]?.id === "actions" ? 1 : 0), 0, source);
  return reordered;
}
