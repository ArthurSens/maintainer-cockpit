const collectionIDPattern = /^[a-z][a-z0-9-]{0,62}$/;

export function presentImportant(active) {
  return {
    active: Boolean(active),
    className: `importance-toggle${active ? " active" : ""}`,
    label: active ? "Remove from Important" : "Mark as Important",
    icon: active ? "⭐" : "☆",
  };
}

export function groupMinePullRequests(pullRequests, viewerID) {
  return {
    authored: pullRequests.filter((pr) => pr.authorID === viewerID),
    important: pullRequests.filter((pr) => pr.personal?.important),
  };
}

export function importantPath(collectionID, pr) {
  const parts = pr.repository?.split("/");
  if (!collectionIDPattern.test(collectionID) ||
      parts?.length !== 2 ||
      !parts[0] ||
      !parts[1] ||
      !Number.isInteger(pr.number) ||
      pr.number <= 0) {
    throw new Error("invalid Important pull request identity");
  }
  return `/api/collections/${encodeURIComponent(collectionID)}/pull-requests/` +
    `${encodeURIComponent(parts[0])}/${encodeURIComponent(parts[1])}/${pr.number}/important`;
}
