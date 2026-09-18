const collectionIDPattern = /^[a-z][a-z0-9-]{0,62}$/;

export function parseLocation(pathname) {
  if (pathname === "/") {
    return { page: "collections" };
  }
  if (pathname === settingsPath()) {
    return { page: "settings" };
  }
  if (pathname === adminPath()) {
    return { page: "admin" };
  }
  const authorMatch = pathname.match(/^\/collections\/([^/]+)\/authors\/([^/]+)$/);
  if (authorMatch) {
    try {
      const collectionID = decodeURIComponent(authorMatch[1]);
      const login = decodeURIComponent(authorMatch[2]);
      if (collectionIDPattern.test(collectionID) && login) {
        return { page: "author", collectionID, login };
      }
    } catch {
      return null;
    }
    return null;
  }
  const match = pathname.match(/^\/collections\/([^/]+)$/);
  if (!match) {
    return null;
  }
  let collectionID;
  try {
    collectionID = decodeURIComponent(match[1]);
  } catch {
    return null;
  }
  if (!collectionIDPattern.test(collectionID)) {
    return null;
  }
  return { page: "collection", collectionID };
}

export function settingsPath() {
  return "/settings";
}

export function adminPath() {
  return "/admin";
}

export function navigationTarget(url) {
  return `${url.pathname}${url.search}${url.hash}`;
}

export function authorPath(collectionID, login) {
  const collection = collectionPath(collectionID);
  if (!login || login.includes("/")) {
    throw new Error("invalid author login");
  }
  return `${collection}/authors/${encodeURIComponent(login)}`;
}

export function collectionPath(collectionID) {
  if (!collectionIDPattern.test(collectionID)) {
    throw new Error("invalid collection ID");
  }
  const url = new URL("https://maintainer-cockpit.invalid");
  url.pathname = `/collections/${encodeURIComponent(collectionID)}`;
  return url.pathname;
}
