export function visibleMembers(members, showHistorical) {
  return showHistorical
    ? [...members]
    : members.filter((member) => member.state === "open");
}

function truncateLabel(value, maxLength = 25) {
  return value.length > maxLength ? `${value.slice(0, maxLength - 1)}…` : value;
}

export function memberLabelLines(member) {
  const slash = member.repository.indexOf("/");
  if (slash < 0) {
    return [truncateLabel(member.repository), `#${member.number}`];
  }
  return [
    truncateLabel(`${member.repository.slice(0, slash)}/`),
    truncateLabel(`${member.repository.slice(slash + 1)} #${member.number}`),
  ];
}

export function edgePresentation(type) {
  return {
    label: type.replaceAll("_", " "),
    directed: type === "stacked_on_top_of",
  };
}

export function clampGraphZoom(value) {
  return Math.min(2.5, Math.max(0.6, value));
}

export function clampNodePosition(position, width, height) {
  return {
    x: Math.min(width - 105, Math.max(105, position.x)),
    y: Math.min(height - 32, Math.max(32, position.y)),
  };
}

export function memberRelationshipSummaries(memberID, edges, memberNames = {}) {
  return edges.flatMap((edge) => {
    if (edge.from !== memberID && edge.to !== memberID) {
      return [];
    }
    const presentation = edgePresentation(edge.type);
    const outgoing = edge.from === memberID;
    const otherID = outgoing ? edge.to : edge.from;
    const other = memberNames[otherID] || otherID;
    const connector = presentation.directed
      ? (outgoing ? "→" : "←")
      : "—";
    return [`${presentation.label} ${connector} ${other}: ${edge.reason}`];
  });
}

export function isPointOutsideRect(point, rect) {
  return point.x < rect.left || point.x > rect.right ||
    point.y < rect.top || point.y > rect.bottom;
}

export function graphLayout(members, width, height) {
  const positions = new Map();
  if (!members.length) {
    return positions;
  }
  const centerX = width / 2;
  const centerY = height / 2;
  const radiusX = Math.max(80, width / 2 - 110);
  const radiusY = Math.max(60, height / 2 - 65);
  members.forEach((member, index) => {
    const angle = -Math.PI / 2 + (2 * Math.PI * index) / members.length;
    positions.set(member.sourceID, {
      x: centerX + radiusX * Math.cos(angle),
      y: centerY + radiusY * Math.sin(angle),
    });
  });
  return positions;
}

export function groupDetailPath(collectionID, groupID) {
  return `/api/collections/${encodeURIComponent(collectionID)}/groups/${encodeURIComponent(groupID)}`;
}
