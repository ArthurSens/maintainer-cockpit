import {
  adminPath,
  authorPath,
  collectionPath,
  navigationTarget,
  parseLocation,
  settingsPath,
} from "./router.js";
import { activeFilterKeys, updateFilters } from "./filters.js";
import {
  groupHiddenPullRequests,
  hiddenChoiceRequest,
  hiddenPath,
  hiddenRequest,
  presentHidden,
} from "./hidden.js";
import {
  groupMinePullRequests,
  importantPath,
  presentImportant,
} from "./important.js";
import {
  clampGraphZoom,
  clampNodePosition,
  edgePresentation,
  graphLayout,
  groupDetailPath,
  isPointOutsideRect,
  memberLabelLines,
  memberRelationshipSummaries,
  visibleMembers,
} from "./groups.js";
import { isRowActivation } from "./row-action.js";
import { advanceSorting, parseSorting, writeSorting } from "./sorting.js";
import { attachFloatingTooltip } from "./tooltip.js";
import {
  defaultTableLayout,
  normalizeTableLayout,
  reorderTableColumn,
  toggleTableColumn,
  visibleTableColumns,
} from "./table-columns.js";
import {
  humanizeAge,
  humanizeBytes,
  presentAnalysis,
  presentAnalysisOperations,
  presentChurn,
  presentContribution,
  presentDecisionBrief,
  presentFinding,
  presentPullRequest,
  presentQuality,
  presentRefresh,
  presentRouteWarnings,
  presentGitHubRoutes,
  presentSchedules,
  safeGitHubURL,
} from "./view-model.js";

const app = document.getElementById("app");
const collectionControls = document.getElementById("collection-controls");
const contextControls = document.getElementById("context-controls");
const viewerControls = document.getElementById("viewer-controls");
let filterFocus = null;
let viewer = { authenticated: false };
const tableLayoutStorageKey = "maintainer-cockpit-table-layout-v1";
let tablePickerOpen = false;
let tablePickerFocus = null;

function element(tag, attributes = {}, children = []) {
  const node = document.createElement(tag);
  for (const [name, value] of Object.entries(attributes)) {
    if (value === undefined || value === null) {
      continue;
    }
    if (name === "text") {
      node.textContent = value;
    } else if (name === "class") {
      node.className = value;
    } else {
      node.setAttribute(name, value);
    }
  }
  node.append(...children);
  return node;
}

function churnInline(additions, deletions, { raw = false } = {}) {
  const churn = presentChurn(additions, deletions);
  return element("span", {
    class: "churn",
    "aria-label": `${churn.label}${raw ? "; raw GitHub totals" : ""}`,
  }, [
    element("span", { class: "churn-additions", text: churn.additions }),
    element("span", { class: "churn-deletions", text: churn.deletions }),
    ...(raw ? [element("span", { class: "churn-raw", text: "raw" })] : []),
  ]);
}

function qualityBadge(quality, { detail = false } = {}) {
  const view = presentQuality(quality);
  const children = [element("span", { text: view.label })];
  if (detail && view.detail) {
    children.push(element("small", { text: ` ${view.detail}` }));
  }
  return element("span", {
    class: `quality-badge quality-${view.level}`,
    "aria-label": view.detail ? `${view.label}; ${view.detail}` : view.label,
  }, children);
}

function analysisBadge(analysis) {
  const view = presentAnalysis(analysis);
  const children = [element("span", { text: view.label })];
  if (view.explanation) {
    const help = element("button", {
      type: "button",
      class: "analysis-help",
      "aria-label": `Review cognitive load details: ${view.explanation}`,
    }, [element("span", { "aria-hidden": "true", text: "?" })]);
    attachFloatingTooltip(help, view.explanation);
    children.push(help);
  }
  return element("span", {
    class: `analysis-badge analysis-${view.level}`,
    "aria-label": `Review cognitive load: ${view.label}`,
  }, children);
}

function apiURL(pathname) {
  return new URL(pathname, window.location.origin);
}

async function loadJSON(pathname) {
  const response = await fetch(apiURL(pathname), {
    headers: { Accept: "application/json" },
  });
  if (!response.ok) {
    const payload = await response.json().catch(() => ({}));
    throw new Error(payload.error || `Request failed with status ${response.status}`);
  }
  return response.json();
}

async function mutate(pathname, method) {
  const response = await fetch(apiURL(pathname), {
    method,
    headers: { "X-CSRF-Token": viewer.csrfToken },
  });
  if (!response.ok) {
    const payload = await response.json().catch(() => ({}));
    throw new Error(payload.error || `Request failed with status ${response.status}`);
  }
}

async function mutateJSON(pathname, value, method = "PUT") {
  const response = await fetch(apiURL(pathname), {
    method,
    headers: {
      "Content-Type": "application/json",
      "X-CSRF-Token": viewer.csrfToken,
    },
    body: JSON.stringify(value),
  });
  const payload = await response.json().catch(() => ({}));
  if (!response.ok) {
    throw new Error(payload.error || `Request failed with status ${response.status}`);
  }
  return payload;
}

function authorizedFor(collectionID) {
  return viewer.authenticated &&
    (viewer.authorizedCollections || []).includes(collectionID);
}

function loadTableLayout(showActions) {
  let saved = null;
  try {
    saved = JSON.parse(window.localStorage.getItem(tableLayoutStorageKey));
  } catch {
    // A focused default remains available without browser storage.
  }
  return normalizeTableLayout(saved, { showActions });
}

function saveTableLayout(layout) {
  try {
    window.localStorage.setItem(
      tableLayoutStorageKey,
      JSON.stringify(layout.map(({ id, visible }) => ({ id, visible }))),
    );
  } catch {
    // Layout changes still apply to the current render when storage is unavailable.
  }
}

function renderTodayProgress(collectionID, progress, active) {
  if (!progress) {
    contextControls.replaceChildren();
    return;
  }
  contextControls.replaceChildren(element("a", {
    class: `today-progress${active ? " active" : ""}`,
    href: `${collectionPath(collectionID)}?view=today`,
    "data-route": "",
    "aria-label": `${progress.count} of ${progress.target} progressed pull requests today`,
  }, [
    element("strong", { text: `${progress.count}/${progress.target}` }),
    element("span", { text: "Today" }),
  ]));
}

function renderCollectionSwitcher(collectionID, collections) {
  const current = collections.find((collection) => collection.id === collectionID);
  if (!current) {
    collectionControls.replaceChildren();
    return;
  }
  const search = element("input", {
    type: "search",
    placeholder: "Search collections",
    "aria-label": "Search collections",
  });
  const empty = element("p", {
    class: "collection-switcher-empty",
    text: "No collections found.",
    hidden: "",
  });
  const items = collections.map((collection) => {
    const active = collection.id === collectionID;
    return element("li", { "data-name": collection.name.toLocaleLowerCase() }, [
      element("a", {
        href: collectionPath(collection.id),
        "data-route": "",
        "aria-current": active ? "page" : null,
      }, [
        element("span", {
          class: "collection-switcher-check",
          "aria-hidden": "true",
          text: active ? "✓" : "",
        }),
        element("span", { class: "collection-switcher-name", text: collection.name }),
        element("span", {
          class: "collection-switcher-count",
          text: String(collection.openPRs),
          title: `${collection.openPRs} open pull requests`,
        }),
      ]),
    ]);
  });
  const list = element("ul", { class: "collection-switcher-list" }, items);
  const switcher = element("details", { class: "collection-switcher" }, [
    element("summary", { "aria-label": "Switch collection" }, [
      element("strong", { text: current.name }),
      element("span", { "aria-hidden": "true", text: "▾" }),
    ]),
    element("div", { class: "collection-switcher-popover" }, [
      element("div", { class: "collection-switcher-search" }, [search]),
      list,
      empty,
    ]),
  ]);
  search.addEventListener("input", () => {
    const term = search.value.trim().toLocaleLowerCase();
    let visible = 0;
    items.forEach((item) => {
      item.hidden = !item.getAttribute("data-name").includes(term);
      visible += item.hidden ? 0 : 1;
    });
    empty.hidden = visible !== 0;
  });
  search.addEventListener("keydown", (event) => {
    if (event.key === "Escape") {
      switcher.open = false;
      switcher.querySelector("summary").focus();
    }
  });
  switcher.addEventListener("toggle", () => {
    if (switcher.open) {
      search.focus();
    }
  });
  collectionControls.replaceChildren(
    element("span", { class: "collection-separator", "aria-hidden": "true", text: "/" }),
    switcher,
  );
}

function collectionSidebar(collectionID, query, showPrivate) {
  const sidebar = element("aside", { class: "collection-sidebar" });
  let collapsed = false;
  try {
    collapsed = window.localStorage.getItem("collection-sidebar-collapsed") === "true";
  } catch {
    // Storage may be unavailable in privacy-restricted browser contexts.
  }
  sidebar.classList.toggle("sidebar-collapsed", collapsed);

  const toggle = element("button", {
    type: "button",
    class: "sidebar-toggle",
    "aria-label": collapsed ? "Expand collection menu" : "Collapse collection menu",
    "aria-expanded": String(!collapsed),
  }, [
    element("span", { "aria-hidden": "true", text: "☰" }),
  ]);
  toggle.addEventListener("click", () => {
    const next = !sidebar.classList.contains("sidebar-collapsed");
    sidebar.classList.toggle("sidebar-collapsed", next);
    toggle.setAttribute("aria-expanded", String(!next));
    toggle.setAttribute("aria-label", next ? "Expand collection menu" : "Collapse collection menu");
    try {
      window.localStorage.setItem("collection-sidebar-collapsed", String(next));
    } catch {
      // The menu remains usable without persisted browser storage.
    }
  });

  const links = [
    {
      href: collectionPath(collectionID),
      icon: "≡",
      label: "All pull requests",
      active: !query.get("view"),
    },
    {
      href: `${collectionPath(collectionID)}?view=groups`,
      icon: "🕸️",
      label: "Groups",
      active: query.get("view") === "groups",
    },
    ...(showPrivate ? [
    {
      href: `${collectionPath(collectionID)}?view=mine`,
      icon: "⭐",
      label: "Mine",
      active: query.get("view") === "mine",
    },
    {
      href: `${collectionPath(collectionID)}?view=hidden`,
      icon: "🙈",
      label: "Snoozed / Ignored",
      active: query.get("view") === "hidden",
    },
    ] : []),
  ].map((item) => element("a", {
    href: item.href,
    "data-route": "",
    class: item.active ? "active" : "",
    "aria-current": item.active ? "page" : null,
    title: item.label,
  }, [
    element("span", {
      class: `sidebar-icon${item.icon === "🙈" ? " sidebar-icon-emoji" : ""}`,
      "aria-hidden": "true",
      text: item.icon,
    }),
    element("span", { class: "sidebar-label", text: item.label }),
  ]));

  sidebar.append(
    element("div", { class: "sidebar-header" }, [toggle]),
    element("nav", { "aria-label": "Collection views" }, links),
  );
  return sidebar;
}

function importanceToggle(collectionID, pr) {
  const view = presentImportant(pr.personal?.important);
  const button = element("button", {
    type: "button",
    class: view.className,
    title: view.label,
    "aria-label": `${view.label}: ${pr.title}`,
    "aria-pressed": String(view.active),
  }, [
    element("span", {
      class: "importance-star",
      "aria-hidden": "true",
      text: view.icon,
    }),
  ]);
  button.addEventListener("click", async (event) => {
    event.stopPropagation();
    button.disabled = true;
    try {
      await mutate(importantPath(collectionID, pr), view.active ? "DELETE" : "PUT");
      render();
    } catch (error) {
      button.disabled = false;
      button.setAttribute("title", error.message);
      button.setAttribute("aria-label", `Unable to update Important: ${error.message}`);
    }
  });
  return button;
}

function renderViewer() {
  if (!viewer.authenticated) {
    viewerControls.replaceChildren(element("a", {
      class: "auth-button",
      href: "/auth/login",
      text: "Sign in with GitHub",
    }));
    return;
  }
  const logout = element("button", {
    class: "account-menu-item",
    type: "button",
    text: "Sign out",
  });
  logout.addEventListener("click", async () => {
    const response = await fetch("/auth/logout", {
      method: "POST",
      headers: { "X-CSRF-Token": viewer.csrfToken },
    });
    if (response.ok) {
      window.location.assign("/");
    }
  });
  const deleteData = element("button", {
    class: "account-menu-item account-menu-danger",
    type: "button",
    text: "Delete my data",
  });
  deleteData.addEventListener("click", async () => {
    if (!window.confirm("Delete all of your Maintainer Cockpit personal data?")) {
      return;
    }
    const response = await fetch("/api/me/data", {
      method: "DELETE",
      headers: { "X-CSRF-Token": viewer.csrfToken },
    });
    if (response.ok) {
      window.location.assign("/");
    }
  });
  const avatar = viewer.avatarURL
    ? element("img", {
        class: "account-avatar",
        src: viewer.avatarURL,
        alt: "",
        width: "28",
        height: "28",
      })
    : element("span", {
        class: "account-avatar",
        "aria-hidden": "true",
        text: viewer.login.slice(0, 1).toUpperCase(),
      });
  const accountMenu = element("details", { class: "account-menu" }, [
    element("summary", {
      class: "account-trigger",
      "aria-label": `Open account menu for ${viewer.login}`,
    }, [
      avatar,
      element("span", { class: "account-login", text: viewer.login }),
      element("span", { class: "account-caret", "aria-hidden": "true", text: "▾" }),
    ]),
    element("div", { class: "account-popover" }, [
      element("div", { class: "account-heading" }, [
        element("small", { text: "Signed in as" }),
        element("strong", { text: viewer.login }),
      ]),
      element("a", {
        class: "account-menu-item",
        href: settingsPath(),
        "data-route": "",
        text: "Goal settings",
      }),
      ...(viewer.deploymentAdmin ? [element("a", {
        class: "account-menu-item",
        href: adminPath(),
        "data-route": "",
        text: "Deployment status",
      })] : []),
      logout,
      deleteData,
    ]),
  ]);
  viewerControls.replaceChildren(accountMenu);
}

function navigate(event) {
  const link = event.target.closest("[data-route]");
  if (!link) {
    return;
  }
  event.preventDefault();
  const url = new URL(link.href);
  window.history.pushState({}, "", navigationTarget(url));
  render();
}

function refreshStatus(collection) {
  const refresh = presentRefresh(collection);
  const details = [];
  if (refresh.timestamp) {
    details.push(`Last complete refresh ${new Date(refresh.timestamp).toLocaleString()}.`);
  }
  if (refresh.message) {
    details.push(refresh.message);
  }
  return element("div", {
    class: `refresh-status refresh-${refresh.state}${refresh.limited ? " refresh-limited" : ""}`,
    role: refresh.warning ? "alert" : "status",
  }, [
    element("strong", { text: refresh.label }),
    ...(details.length ? [element("span", { text: details.join(" ") })] : []),
  ]);
}

function routeWarningsControl(collection) {
  const warnings = presentRouteWarnings(collection.routeWarnings);
  if (!warnings.visible) {
    return null;
  }
  return element("details", {
    class: "route-warnings",
    open: warnings.disclosure.open ? "" : null,
  }, [
    element("summary", {}, [
      element("strong", { text: warnings.summary }),
    ]),
    element("div", { role: warnings.disclosure.alertRole }, [
      element("p", { text: warnings.guidance }),
      element("ul", {}, warnings.repositories.map((repository) =>
        element("li", {}, [
          element("strong", { text: repository.repository }),
          element("ul", {}, repository.operations.map((operation) =>
            element("li", {}, [
              element("span", { text: `${operation.label}: ${operation.message}` }),
              element("small", { text: operation.statusLine }),
            ]))),
        ]))),
    ]),
  ]);
}

async function renderCollections() {
  const data = await loadJSON("/api/collections");
  const cards = data.collections.map((collection) => {
    const path = collectionPath(collection.id);
    return element("article", { class: "collection-card" }, [
      element("div", { class: "collection-copy" }, [
        element("h2", {}, [
          element("a", { href: path, "data-route": "", text: collection.name }),
        ]),
        element("p", { text: collection.description || "No description provided." }),
        element("p", {
          class: "meta",
          text: `${collection.repositories.length} repositories`,
        }),
        refreshStatus(collection),
      ]),
      element("strong", {
        class: "count",
        text: `${collection.openPRs} open PR${collection.openPRs === 1 ? "" : "s"}`,
      }),
    ]);
  });
  app.replaceChildren(
    element("section", { class: "page-heading" }, [
      element("p", { class: "eyebrow", text: "Collections" }),
      element("h1", { text: "Maintainer backlogs" }),
      element("p", {
        class: "lede",
        text: "Choose a collection to inspect its persisted pull requests.",
      }),
    ]),
    ...(cards.length
      ? cards
      : [element("p", { class: "empty", text: "No collections are configured." })]),
  );
}

function openHiddenStateDialog(collectionID, pr) {
  const dialog = element("dialog", {
    class: "hidden-state-dialog",
    "aria-labelledby": "hidden-dialog-title",
  });
  const form = element("form", { class: "hidden-form" }, [
    element("h2", {
      id: "hidden-dialog-title",
      text: "Hide pull request",
    }),
    element("fieldset", { class: "hidden-choices" }, [
      element("legend", { text: "How long should it stay hidden?" }),
      element("label", {}, [
        element("input", {
          type: "radio", name: "choice", value: "forever",
        }),
        element("span", { text: "Ignore forever" }),
      ]),
      element("label", {}, [
        element("input", {
          type: "radio", name: "choice", value: "activity", checked: "",
        }),
        element("span", { text: "Until next activity" }),
      ]),
      element("label", {}, [
        element("input", {
          type: "radio", name: "choice", value: "period",
        }),
        element("span", { text: "For a time period" }),
      ]),
    ]),
    element("label", { class: "hidden-period", hidden: "" }, [
      element("span", { text: "Wake date and time" }),
      element("input", {
        type: "datetime-local",
        name: "customUntil",
        min: localDateTimeValue(new Date()),
        value: localDateTimeValue(new Date(Date.now() + 24 * 60 * 60 * 1000)),
      }),
    ]),
    element("label", {}, [
      element("span", { text: "Additional notes (optional)" }),
      element("textarea", {
        name: "reason", maxlength: "500", rows: "3",
      }),
    ]),
    element("div", { class: "hidden-actions" }, [
      element("button", { type: "submit", text: "Hide pull request" }),
      element("button", {
        type: "button", class: "secondary cancel-hidden", text: "Cancel",
      }),
    ]),
    element("p", { class: "hidden-status" }),
  ]);
  const choices = [...form.querySelectorAll('[name="choice"]')];
  const period = form.querySelector(".hidden-period");
  const submit = form.querySelector('button[type="submit"]');
  choices.forEach((choice) => choice.addEventListener("change", () => {
    period.hidden = choice.value !== "period";
  }));
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const values = new FormData(form);
    const status = form.querySelector(".hidden-status");
    submit.disabled = true;
    try {
      await mutateJSON(hiddenPath(collectionID, pr), hiddenChoiceRequest({
        choice: values.get("choice"),
        snoozedUntil: values.get("customUntil"),
        reason: values.get("reason"),
      }));
      dialog.close();
      render();
    } catch (error) {
      submit.disabled = false;
      status.textContent = error.message;
      status.setAttribute("role", "alert");
    }
  });
  form.querySelector(".cancel-hidden").addEventListener("click", () => dialog.close());
  dialog.append(form);
  dialog.addEventListener("close", () => dialog.remove(), { once: true });
  document.body.append(dialog);
  dialog.showModal();
  choices.find((choice) => choice.checked)?.focus();
}

function rowHiddenActions(collectionID, pr) {
  const button = element("button", {
    type: "button",
    class: "hide-row-button",
    text: "🙈",
    title: "Hide pull request",
    "aria-label": `Hide pull request: ${pr.title}`,
  });
  button.addEventListener("click", (event) => {
    event.stopPropagation();
    openHiddenStateDialog(collectionID, pr);
  });
  return button;
}

function prRow(collectionID, pr, columns) {
  const view = presentPullRequest(pr);
  const hidden = presentHidden(pr.personal?.hidden);
  const contribution = presentContribution(pr.contribution);
  const modelAnalysis = presentAnalysis(pr.analysis);
  const number = view.githubURL
    ? element("a", {
        href: view.githubURL,
        target: "_blank",
        rel: "noreferrer",
        text: view.number,
      })
    : element("span", { text: view.number });
  const cells = {
    actions: () => element("td", { class: "actions-cell" }, [
      importanceToggle(collectionID, pr),
      rowHiddenActions(collectionID, pr),
    ]),
    number: () => element("td", {}, [number]),
    repository: () => element("td", { text: view.repository }),
    title: () => element("td", { class: "pr-title-cell", title: view.title }, [
      element("span", { text: view.title }),
      ...(hidden.active ? [
        element("span", {
          class: `hidden-badge hidden-${hidden.kind}`,
          text: hidden.label,
        }),
      ] : []),
      ...(hidden.newActivity ? [
        element("span", { class: "new-activity-badge", text: "New activity" }),
      ] : []),
      ...(hidden.reason ? [
        element("small", { class: "hidden-reason", text: hidden.reason }),
      ] : []),
    ]),
    contribution: () => element("td", {
      title: contribution.available ? contribution.evidence : contribution.label,
    }, [
      element("a", {
        href: authorPath(collectionID, pr.author),
        "data-route": "",
        class: `contribution-badge contribution-${contribution.level || "incomplete"}`,
        text: contribution.label,
        "aria-label": `${view.author}: ${contribution.label}`,
      }),
    ]),
    churn: () => element("td", {
      title: `${view.size}; ${view.changedFiles}`,
    }, [
      churnInline(pr.additions, pr.deletions),
      element("small", { class: "changed-files", text: view.changedFiles }),
    ]),
    quality: () => element("td", {}, [qualityBadge(pr.quality, { detail: true })]),
    review_load: () => element("td", {
      class: "review-load-cell",
    }, [analysisBadge(pr.analysis)]),
    waiting: () => element("td", {
      class: "waiting-badges",
      text: modelAnalysis.waitingLabels.length
        ? modelAnalysis.waitingLabels.join(", ")
        : "None detected",
    }),
    review: () => element("td", { text: view.reviewState }),
    updated: () => element("td", {
      text: humanizeAge(view.updatedAt),
      title: new Date(view.updatedAt).toLocaleString(),
    }),
  };
  return element("tr", {
    class: "clickable-row",
    tabindex: "0",
    "aria-haspopup": "dialog",
    "aria-label": `Open details for ${view.repository} ${view.number}: ${view.title}`,
  }, columns.map(({ id }) => cells[id]()));
}

function listControls(query, repositories, resultCount, tableControl) {
  const search = element("input", {
    name: "q",
    value: query.get("q") || "",
    maxlength: "200",
    placeholder: "Search title, author, repository, or PR number",
  });
  const activeFilters = element("div", { class: "active-filters" });
  const form = element("form", { class: "list-controls", role: "search" }, [
    element("label", { class: "search-filter" }, [
      element("span", { text: "Search" }),
      search,
    ]),
    activeFilters,
  ]);
  const apply = (restoreSearchFocus = false) => {
    if (restoreSearchFocus) {
      filterFocus = {
        name: "q",
        start: search.selectionStart,
        end: search.selectionEnd,
      };
    }
    const next = updateFilters(
      new URLSearchParams(window.location.search),
      new FormData(form),
    );
    const encoded = next.toString();
    window.history.replaceState(
      {},
      "",
      `${window.location.pathname}${encoded ? `?${encoded}` : ""}`,
    );
    render();
  };

  const definitions = [
    {
      key: "repo",
      label: "Repository",
      options: [
        ["", "Choose repository"],
        ...repositories.map((repository) => [repository, repository]),
      ],
    },
    {
      key: "review",
      label: "Review",
      options: [
        ["", "Choose review state"],
        ["review_requested", "Review requested"],
        ["changes_requested", "Changes requested"],
        ["approved", "Approved"],
        ["draft", "Draft"],
        ["none", "No review"],
      ],
    },
    {
      key: "quality",
      label: "Quality",
      options: [
        ["", "Choose quality"],
        ["no_concerns", "No concerns detected"],
        ["review_suggested", "Review suggested"],
        ["strong_concerns", "Strong concerns"],
      ],
    },
    {
      key: "review_load",
      label: "Review Cognitive Load",
      options: [
        ["", "Choose load"],
        ["low", "Low"],
        ["medium", "Medium"],
        ["high", "High"],
      ],
    },
    {
      key: "analysis_status",
      label: "Analysis status",
      options: [
        ["", "Choose analysis status"],
        ["available", "Available"],
        ["pending", "Pending"],
        ["stale", "Stale"],
        ["partial", "Partial"],
        ["failed", "Failed"],
        ["invalid", "Invalid"],
      ],
    },
    { key: "waiting", label: "Waiting on" },
  ];

  const removeButton = (label, control) => {
    const button = element("button", {
      type: "button",
      class: "remove-filter",
      title: `Remove ${label} filter`,
      "aria-label": `Remove ${label} filter`,
      text: "×",
    });
    button.addEventListener("click", () => {
      control.remove();
      apply();
    });
    return button;
  };

  const waitingControl = (definition) => {
    const selected = new Set((query.get("waiting") || "").split(",").filter(Boolean));
    const picker = element("details", { class: "waiting-picker" });
    const summary = element("summary", {
      text: selected.size ? `${selected.size} selected` : "Choose parties",
    });
    const choices = [
      ["triager", "Triager"],
      ["maintainer", "Maintainer"],
      ["author", "Author"],
      ["external_dependency", "External dependency"],
    ].map(([value, text]) => element("label", {}, [
      element("input", {
        type: "checkbox",
        name: "waiting",
        value,
        checked: selected.has(value) ? "" : null,
      }),
      element("span", { text }),
    ]));
    const mode = element("select", {
      name: "waiting_mode",
      "aria-label": "Waiting filter matching",
    }, [
      ["any", "Match any selected party"],
      ["all", "Match all selected parties"],
    ].map(([value, text]) => element("option", {
      value,
      text,
      selected: (query.get("waiting_mode") || "any") === value ? "" : null,
    })));
    const modeLabel = element("label", { class: "waiting-mode" }, [
      element("span", { text: "Matching" }),
      mode,
    ]);
    const updateWaitingState = () => {
      const count = choices.filter((choice) => choice.querySelector("input").checked).length;
      summary.textContent = count ? `${count} selected` : "Choose parties";
      modeLabel.hidden = count < 2;
      mode.disabled = count < 2;
    };
    choices.forEach((choice) => {
      choice.querySelector("input").addEventListener("change", updateWaitingState);
    });
    updateWaitingState();
    picker.append(
      summary,
      element("div", { class: "waiting-options" }, [
        ...choices,
        modeLabel,
        element("button", { type: "submit", text: "Apply" }),
      ]),
    );
    const control = element("div", { class: "filter-control" }, [
      element("span", { class: "filter-name", text: definition.label }),
      element("div", { class: "filter-input" }, [
        picker,
      ]),
    ]);
    control.querySelector(".filter-input").append(removeButton(definition.label, control));
    return control;
  };

  const selectControl = (definition) => {
    const select = element("select", {
      name: definition.key,
      "aria-label": `${definition.label} filter`,
    }, definition.options.map(([value, text]) => element("option", {
      value,
      text,
      selected: query.get(definition.key) === value ? "" : null,
    })));
    select.addEventListener("change", () => apply());
    const control = element("div", { class: "filter-control" }, [
      element("span", { class: "filter-name", text: definition.label }),
      element("div", { class: "filter-input" }, [
        select,
      ]),
    ]);
    control.querySelector(".filter-input").append(removeButton(definition.label, control));
    return control;
  };

  const addControl = (definition, focus = false) => {
    const control = definition.key === "waiting"
      ? waitingControl(definition)
      : selectControl(definition);
    activeFilters.append(control);
    if (focus) {
      const picker = control.querySelector("details");
      if (picker) {
        picker.open = true;
        picker.querySelector("input")?.focus();
      } else {
        control.querySelector("select")?.focus();
      }
    }
  };

  const applied = new Set(activeFilterKeys(query));
  definitions.filter((definition) => applied.has(definition.key)).forEach(addControl);
  const addFilter = element("details", { class: "add-filter" }, [
    element("summary", { text: "Add filter" }),
    element("div", { class: "add-filter-menu" }),
  ]);
  const menu = addFilter.querySelector(".add-filter-menu");
  definitions.filter((definition) => !applied.has(definition.key)).forEach((definition) => {
    const button = element("button", {
      type: "button",
      text: definition.label,
    });
    button.addEventListener("click", () => {
      addControl(definition, true);
      button.remove();
      addFilter.open = false;
      addFilter.hidden = !menu.children.length;
    });
    menu.append(button);
  });
  addFilter.hidden = !menu.children.length;
  form.append(
    addFilter,
    tableControl,
    element("p", {
      class: "result-count",
      text: `${resultCount} pull request${resultCount === 1 ? "" : "s"} match`,
    }),
  );
  form.addEventListener("submit", (event) => {
    event.preventDefault();
    apply(true);
  });
  let searchTimer;
  search.addEventListener("input", () => {
    window.clearTimeout(searchTimer);
    searchTimer = window.setTimeout(() => apply(true), 300);
  });
  return form;
}

function modifyTableControl(layout, onChange) {
  let draggedID = null;
  const status = element("span", {
    class: "sr-only",
    "aria-live": "polite",
  });
  const apply = (next, message) => {
    saveTableLayout(next);
    status.textContent = message;
    onChange(next);
  };
  const list = element("ul", { class: "table-column-list" });
  layout.forEach((column, index) => {
    const checkbox = element("input", {
      type: "checkbox",
      checked: column.visible ? "" : null,
      disabled: column.fixed ? "" : null,
      "aria-label": `${column.visible ? "Hide" : "Show"} ${column.label} column`,
    });
    checkbox.addEventListener("change", () => {
      tablePickerFocus = { id: column.id, control: "checkbox" };
      apply(toggleTableColumn(layout, column.id), `${column.label} column updated`);
    });
    const row = element("li", {
      class: `table-column-item${column.id === "actions" ? " column-pinned" : ""}`,
      draggable: column.id === "actions" ? "false" : "true",
      "data-column-id": column.id,
    }, [
      element("span", {
        class: "column-drag-handle",
        "aria-hidden": "true",
        text: column.id === "actions" ? "" : "⠿",
      }),
      element("label", {}, [
        checkbox,
        element("span", { text: column.label }),
      ]),
    ]);
    if (column.id !== "actions") {
      const previous = layout[index - 1];
      const next = layout[index + 1];
      const move = (target, direction) => element("button", {
        type: "button",
        class: "column-move",
        text: direction === "up" ? "↑" : "↓",
        title: `Move ${column.label} ${direction}`,
        "aria-label": `Move ${column.label} ${direction}`,
        "data-direction": direction,
        disabled: !target || target.id === "actions" ? "" : null,
      });
      const up = move(previous, "up");
      const down = move(next, "down");
      up.addEventListener("click", () => {
        tablePickerFocus = { id: column.id, control: "up" };
        apply(
          reorderTableColumn(layout, column.id, previous.id),
          `${column.label} moved up`,
        );
      });
      down.addEventListener("click", () => {
        tablePickerFocus = { id: column.id, control: "down" };
        apply(
          reorderTableColumn(layout, column.id, next.id),
          `${column.label} moved down`,
        );
      });
      row.append(element("span", { class: "column-move-buttons" }, [up, down]));
      row.addEventListener("dragstart", () => {
        draggedID = column.id;
        row.classList.add("dragging");
      });
      row.addEventListener("dragend", () => {
        draggedID = null;
        row.classList.remove("dragging");
      });
      row.addEventListener("dragover", (event) => event.preventDefault());
      row.addEventListener("drop", (event) => {
        event.preventDefault();
        if (draggedID && draggedID !== column.id) {
          apply(
            reorderTableColumn(layout, draggedID, column.id),
            `${layout.find(({ id }) => id === draggedID)?.label || "Column"} reordered`,
          );
        }
      });
    }
    list.append(row);
  });
  const details = element("details", {
    class: "modify-table",
    open: tablePickerOpen ? "" : null,
  }, [
    element("summary", { text: "Modify table" }),
    element("div", { class: "modify-table-menu" }, [
      element("p", { class: "meta", text: "Show, hide, or reorder columns." }),
      list,
      element("button", {
        type: "button",
        class: "restore-table-defaults",
        text: "Restore defaults",
      }),
      status,
    ]),
  ]);
  details.addEventListener("toggle", () => {
    tablePickerOpen = details.open;
  });
  details.querySelector(".restore-table-defaults").addEventListener("click", () => {
    tablePickerFocus = { control: "restore" };
    apply(
      defaultTableLayout({ showActions: layout.some(({ id }) => id === "actions") }),
      "Default table columns restored",
    );
  });
  return details;
}

function sortHeader(header, sorting) {
  const priority = sorting.sorts.findIndex((sort) => sort.key === header.key);
  const active = priority >= 0;
  const sort = active ? sorting.sorts[priority] : null;
  const direction = sort?.order === "asc" ? "ascending" : "descending";
  return element("th", {
    scope: "col",
    "aria-sort": active && priority === 0 ? direction : "none",
  }, [
    element("button", {
      type: "button",
      class: `sort-header${active ? " active" : ""}`,
      title: active
        ? `${header.label}: ${direction}, priority ${priority + 1}`
        : `Sort by ${header.label}`,
      "aria-label": active
        ? `${header.label}, sort priority ${priority + 1}, ${direction}`
        : `Sort by ${header.label}`,
    }, [
      element("span", { text: header.label }),
      ...(active ? [
        element("span", {
          class: "sort-direction",
          "aria-hidden": "true",
          text: sort.order === "asc" ? "↑" : "↓",
        }),
        element("span", {
          class: "sort-priority",
          "aria-hidden": "true",
          text: String(priority + 1),
        }),
      ] : []),
    ]),
  ]);
}

function localDateTimeValue(value) {
  if (!value) {
    return "";
  }
  const date = new Date(value);
  const local = new Date(date.getTime() - date.getTimezoneOffset() * 60_000);
  return local.toISOString().slice(0, 16);
}

function renderPullRequestDecisionBrief(dialog, detail) {
  const view = presentPullRequest(detail.pullRequest);
  const brief = presentDecisionBrief(detail);
  const modelAnalysis = presentAnalysis(detail.analysis);
  const contextRelationships = detail.context?.relationships || [];
  const sourceURLs = new Map(
    contextRelationships.map((item) => [item.sourceID, safeGitHubURL(item.url)]),
  );
  sourceURLs.set(
    `PR_${detail.pullRequest.repository.replace("/", "_")}_${detail.pullRequest.number}`,
    view.githubURL,
  );
  for (const file of detail.files || []) {
    sourceURLs.set(`file:${file.path}`, view.githubURL ? `${view.githubURL}/files` : null);
  }
  const evidenceLinks = (sourceIDs, { prefix = true } = {}) =>
    element("small", { class: "evidence-links" }, [
    ...(prefix ? ["Evidence: "] : []),
    ...sourceIDs.flatMap((sourceID, index) => [
      ...(index ? [", "] : []),
      sourceURLs.get(sourceID)
        ? element("a", {
            href: sourceURLs.get(sourceID), target: "_blank",
            rel: "noopener noreferrer", text: sourceID,
          })
        : element("span", { text: sourceID }),
    ]),
  ]);
  const collapsedEvidence = (sourceIDs) => element("details", {
    class: "load-evidence",
  }, [
    element("summary", { text: `Evidence (${sourceIDs.length})` }),
    evidenceLinks(sourceIDs, { prefix: false }),
  ]);
  const statusIcon = (state) => ({
    clear: "✓", blocked: "!", attention: "•", neutral: "–", unknown: "?",
  })[state] || "?";
  const readinessItems = brief.readiness.map((signal) =>
    element("li", { class: `readiness-item readiness-${signal.state}` }, [
      element("span", {
        class: "readiness-icon", text: statusIcon(signal.state), "aria-hidden": "true",
      }),
      element("span", {}, [
        element("strong", { text: {
          draft: "Draft state", review: "Reviews", checks: "Check Runs", conflicts: "Conflicts",
        }[signal.key] }),
        element("small", { text: signal.label }),
      ]),
    ]));

  const requested = [
    ...brief.people.requestedReviewers.map((name) => `@${name}`),
    ...brief.people.requestedTeams.map((name) => `team/${name}`),
  ];
  const peopleChildren = [
    element("div", { class: "people-row" }, [
      element("span", { class: "people-label", text: "Waiting on" }),
      element("span", {
        text: brief.people.waiting.length
          ? brief.people.waiting.map((item) => item.party).join(", ")
          : "Nobody detected",
      }),
    ]),
    element("div", { class: "people-row" }, [
      element("span", { class: "people-label", text: "Requested" }),
      element("span", { text: requested.length ? requested.join(", ") : "Nobody" }),
    ]),
    element("div", { class: "people-row" }, [
      element("span", { class: "people-label", text: "Author" }),
      element("span", {
        text: brief.people.contribution.available
          ? `${view.author} · ${brief.people.contribution.label}`
          : `${view.author} · History unknown`,
      }),
    ]),
    ...(brief.people.contribution.available
      ? [element("small", {
          class: "author-history", text: brief.people.contribution.evidence,
        })]
      : []),
  ];

  const qualityEvidence = (detail.quality?.findings || []).map(presentFinding).map((finding) =>
    element("li", {}, [
      element("strong", { text: finding.rule }),
      element("span", { text: ` · ${finding.severity} · ${finding.provenance}` }),
      element("p", { text: finding.summary }),
      ...(finding.sourceIDs.length ? [evidenceLinks(finding.sourceIDs)] : []),
    ]));
  const checkEvidence = (brief.checks.runs || []).map((run) => {
    const href = safeGitHubURL(run.url);
    const state = run.status === "completed" ? run.conclusion || "unknown" : run.status || "unknown";
    return element("li", {}, [
      href
        ? element("a", {
            href, target: "_blank", rel: "noopener noreferrer", text: run.name || "Check Run",
          })
        : element("strong", { text: run.name || "Check Run" }),
      element("span", { text: ` · ${state.replaceAll("_", " ")}` }),
    ]);
  });
  const waitingEvidence = brief.people.waiting
    .filter((item) => item.sourceIDs.length)
    .map((item) => element("li", {}, [
      element("strong", { text: `Waiting on ${item.party}` }),
      element("p", { text: item.reason }),
      evidenceLinks(item.sourceIDs),
    ]));
  const unusualEvidence = brief.people.unusual
    ? [element("li", { class: "unusual-evidence" }, [
        element("strong", { text: "Experimental unusual author activity" }),
        element("p", { text: brief.people.unusual.detail }),
        element("small", { text: brief.people.unusual.thresholds }),
      ])]
    : [];
  const evidenceItems = [
    ...qualityEvidence, ...waitingEvidence, ...checkEvidence, ...unusualEvidence,
  ];
  const loadComponents = modelAnalysis.components.map((component) =>
    element("li", { class: "load-component" }, [
      element("details", {}, [
        element("summary", { class: "load-component-summary" }, [
          element("strong", { text: component.name }),
          element("span", {
            class: `analysis-badge analysis-${component.level}`, text: component.label,
          }),
        ]),
        element("div", { class: "load-component-content" }, [
          element("p", { text: component.reason }),
          ...(component.completeness
            ? [element("small", {
              class: "component-completeness",
              text: `Completeness: ${component.completeness}` +
                `${component.partialReason ? ` · ${component.partialReason}` : ""}`,
            })]
            : []),
          ...(component.sourceIDs.length ? [collapsedEvidence(component.sourceIDs)] : []),
        ]),
      ]),
    ]));
  const loadMetadata = [
    modelAnalysis.completeness ? `Completeness: ${modelAnalysis.completeness}` : "",
    modelAnalysis.partialReason ? `Partial reason: ${modelAnalysis.partialReason}` : "",
    `Attempt health: ${modelAnalysis.attemptHealth}`,
    modelAnalysis.provenance ? `Provider/model/time: ${modelAnalysis.provenance}` : "",
  ].filter(Boolean);

  dialog.replaceChildren(
    element("div", { class: "drawer-header" }, [
      element("div", {}, [
        element("p", { class: "eyebrow", text: view.repository }),
        element("h2", { id: "drawer-title", text: `${view.number} ${view.title}` }),
        element("p", { class: "drawer-freshness", text: brief.freshness }),
      ]),
      element("button", {
        type: "button", class: "drawer-close", text: "Close", "aria-label": "Close details",
      }),
    ]),
    element("section", { class: "decision-section", "aria-labelledby": "readiness-title" }, [
      element("h3", { id: "readiness-title", text: "Readiness" }),
      element("ul", { class: "readiness-grid" }, readinessItems),
    ]),
    element("details", { class: "decision-section load-detail" }, [
      element("summary", { class: "load-summary" }, [
        element("span", { class: "load-title", text: "Review Cognitive Load" }),
        element("span", {
          class: `analysis-badge analysis-${modelAnalysis.level}`,
          text: modelAnalysis.label,
        }),
      ]),
      element("div", { class: "load-content" }, [
        element("p", {
          text: modelAnalysis.rationale || modelAnalysis.message,
        }),
        ...(modelAnalysis.sourceIDs.length ? [collapsedEvidence(modelAnalysis.sourceIDs)] : []),
        ...(loadComponents.length
          ? [element("ul", { class: "load-components" }, loadComponents)]
          : []),
        element("ul", { class: "load-metadata" },
          loadMetadata.map((item) => element("li", { text: item }))),
      ]),
    ]),
    element("section", { class: "decision-section", "aria-labelledby": "people-title" }, [
      element("h3", { id: "people-title", text: "People" }),
      ...peopleChildren,
    ]),
    element("details", { class: "drawer-evidence" }, [
      element("summary", {
        text: `Evidence${evidenceItems.length ? ` (${evidenceItems.length})` : ""}`,
      }),
      ...(evidenceItems.length
        ? [element("ul", {}, evidenceItems)]
        : [element("p", { text: "No additional supporting evidence was collected." })]),
    ]),
  );
}

async function openPullRequestDetail(collectionID, pr, trigger) {
  const [owner, repository] = pr.repository.split("/");
  const path = `/api/collections/${encodeURIComponent(collectionID)}/pull-requests/` +
    `${encodeURIComponent(owner)}/${encodeURIComponent(repository)}/${pr.number}`;
  const dialog = element("dialog", { class: "detail-drawer", "aria-labelledby": "drawer-title" }, [
    element("p", { class: "drawer-loading", text: "Loading pull request context…" }),
  ]);
  trigger.classList.add("selected");
  document.body.append(dialog);
  dialog.addEventListener("close", () => {
    trigger.classList.remove("selected");
    dialog.remove();
    trigger.focus();
    const current = new URLSearchParams(window.location.search);
    const identity = `${pr.repository}#${pr.number}`;
    if (current.get("pr") === identity) {
      current.delete("pr");
      const encoded = current.toString();
      window.history.replaceState(
        {},
        "",
        `${window.location.pathname}${encoded ? `?${encoded}` : ""}`,
      );
    }
  }, { once: true });
  dialog.showModal();
  try {
    const detail = await loadJSON(path);
    renderPullRequestDecisionBrief(dialog, detail);
    dialog.querySelector(".drawer-close").addEventListener("click", () => dialog.close());
    dialog.querySelector(".drawer-close").focus();
  } catch (error) {
    dialog.replaceChildren(
      element("h2", { id: "drawer-title", text: "Unable to load details" }),
      element("p", { role: "alert", text: error.message }),
      element("button", { type: "button", class: "drawer-close", text: "Close" }),
    );
    dialog.querySelector(".drawer-close").addEventListener("click", () => dialog.close());
  }
}

async function renderAuthor(collectionID, login) {
  const data = await loadJSON(
    `/api/collections/${encodeURIComponent(collectionID)}/authors/${encodeURIComponent(login)}`,
  );
  const contribution = presentContribution(data.contribution);
  const policy = data.contribution.policy || {};
  const unusualPolicy = policy.unusualActivity || {};
  const threshold = (item) => `${item?.value ?? "unknown"} (${item?.source || "unknown"})`;
  const rows = Object.entries(data.contribution.repositories || {}).map(([repository, history]) =>
    element("tr", {}, [
      element("td", { text: repository }),
      element("td", { text: history.association || "NONE" }),
      element("td", { text: String(history.merged) }),
      element("td", { text: String(history.closedUnmerged) }),
      element("td", { text: String(history.open) }),
    ]));
  app.replaceChildren(
    element("a", {
      href: collectionPath(collectionID), "data-route": "", class: "back",
      text: "Back to collection",
    }),
    element("section", { class: "page-heading author-heading" }, [
      element("p", { class: "eyebrow", text: "Public contribution context" }),
      element("h1", { text: data.login }),
      element("p", {
        class: `contribution-level contribution-${contribution.level || "incomplete"}`,
        text: contribution.label,
      }),
      element("p", {
        class: "lede",
        text: contribution.available
          ? contribution.evidence
          : "No author judgment is shown because collection history is incomplete.",
      }),
      ...(data.accountCreatedAt && !Number.isNaN(new Date(data.accountCreatedAt).getTime())
        ? [element("p", {
            class: "meta",
            text: `GitHub account created ${new Date(data.accountCreatedAt).toLocaleDateString()}.`,
          })]
        : []),
      ...(contribution.unusual
        ? [element("p", { class: "experimental-signal", text: contribution.unusual })]
        : []),
    ]),
    element("section", {}, [
      element("h2", { text: "Repository evidence" }),
      element("div", { class: "table-frame" }, [
        element("table", {}, [
          element("thead", {}, [element("tr", {}, [
            "Repository", "Association", "Merged", "Closed unmerged", "Open",
          ].map((text) => element("th", { scope: "col", text })))]),
          element("tbody", {}, rows),
        ]),
      ]),
    ]),
    element("section", { class: "policy-card" }, [
      element("h2", { text: "Local formula and thresholds" }),
      element("p", {
        text: `Contribution history and experimental activity evidence refresh every ` +
          `${threshold(policy.refreshIntervalDays)} days.`,
      }),
      element("p", {
        text: `Established: OWNER, MEMBER, or COLLABORATOR association, or ` +
          `${threshold(policy.establishedMergedPRs)} merged PRs.`,
      }),
      element("p", {
        text: `Experimental unusual activity requires an account under ` +
          `${threshold(unusualPolicy.accountAgeDays)} days, activity in at least ` +
          `${threshold(unusualPolicy.minRepositories)} repositories and ` +
          `${threshold(unusualPolicy.minOrganizations)} organizations within ` +
          `${threshold(unusualPolicy.windowDays)} days.`,
      }),
      element("p", {
        class: "meta",
        text: `Collected ${new Date(data.collectedAt).toLocaleString()}. ` +
          "This signal never changes PR quality or contribution level.",
      }),
    ]),
  );
}

const qualifyingActivityLabels = {
  review: "Submitted review",
  comment: "Comment",
  thread_resolved: "Resolved review thread",
  merge: "Merge",
  close: "Close",
};

async function loadProgress(collectionID) {
  const pathname = `/api/collections/${encodeURIComponent(collectionID)}/progress`;
  const progress = await loadJSON(pathname);
  if (progress.configured) {
    return progress;
  }
  const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone;
  if (!timezone || timezone === progress.timezone) {
    return progress;
  }
  return mutateJSON(pathname, {
    target: progress.target,
    enabledActivities: progress.enabledActivities,
    timezone,
  });
}

function progressSummary(progress) {
  const collected = progress.lastCollectionTime
    ? `Last GitHub collection ${new Date(progress.lastCollectionTime).toLocaleString()}.`
    : "GitHub activity has not been collected yet.";
  return element("section", {
    class: "progress-summary",
    "aria-label": "Today's private progress",
  }, [
    element("strong", { text: `${progress.count} of ${progress.target} progressed PRs today` }),
    element("span", { text: `${progress.date} in ${progress.timezone}. ${collected}` }),
  ]);
}

function todayView(progress) {
  const items = progress.pullRequests.map((pr) => {
    const pullRequestURL = safeGitHubURL(pr.url);
    return element("li", { class: "today-item" }, [
      element("h2", {}, [
        ...(pullRequestURL
          ? [element("a", {
              href: pullRequestURL,
              target: "_blank",
              rel: "noopener noreferrer",
              text: `${pr.repository}#${pr.number} ${pr.title}`,
            })]
          : [element("span", { text: `${pr.repository}#${pr.number} ${pr.title}` })]),
      ]),
      element("ul", { class: "today-activities" }, pr.activities.map((activity) => {
        const activityURL = safeGitHubURL(activity.url);
        const label = `${qualifyingActivityLabels[activity.type] || activity.type} at ` +
          new Date(activity.occurredAt).toLocaleString();
        return element("li", {}, [
          ...(activityURL
            ? [element("a", {
                href: activityURL,
                target: "_blank",
                rel: "noopener noreferrer",
                text: label,
              })]
            : [element("span", { text: label })]),
        ]);
      })),
    ]);
  });
  return element("section", { class: "today-view" }, [
    element("h2", { text: "Counted today" }),
    ...(items.length
      ? [element("ul", { class: "today-list" }, items)]
      : [element("p", {
          class: "empty",
          text: "No qualifying activities have been collected for today.",
        })]),
  ]);
}

async function renderSettings() {
  if (!viewer.authenticated) {
    throw new Error("Sign in with GitHub to change private goal settings.");
  }
  const collections = (await loadJSON("/api/collections")).collections
    .filter((collection) => authorizedFor(collection.id));
  const rows = await Promise.all(collections.map(async (collection) => ({
    collection,
    progress: await loadProgress(collection.id),
  })));
  const detectedTimezone = Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  const supportedTimezones = Intl.supportedValuesOf
    ? ["UTC", ...Intl.supportedValuesOf("timeZone").filter((zone) => zone !== "UTC")]
    : ["UTC", detectedTimezone];
  const cards = rows.map(({ collection, progress }) => {
    const timezone = progress.configured ? progress.timezone : detectedTimezone;
    const status = element("p", { class: "settings-status meta" });
    const target = element("input", {
      type: "number", name: "target", min: "1", max: "1000",
      required: "", value: String(progress.target),
    });
    const timezoneSelect = element("select", { name: "timezone", required: "" },
      [...new Set([...supportedTimezones, timezone])].map((zone) => element("option", {
        value: zone, text: zone, selected: zone === timezone ? "" : null,
      })));
    const activities = Object.entries(qualifyingActivityLabels).map(([value, label]) =>
      element("label", { class: "activity-choice" }, [
        element("input", {
          type: "checkbox",
          name: "enabledActivities",
          value,
          checked: progress.enabledActivities.includes(value) ? "" : null,
        }),
        element("span", { text: label }),
      ]));
    const form = element("form", { class: "goal-settings-form" }, [
      element("label", {}, [
        element("span", { text: "Daily target" }),
        target,
      ]),
      element("label", {}, [
        element("span", { text: "Preferred timezone" }),
        timezoneSelect,
      ]),
      element("fieldset", {}, [
        element("legend", { text: "Qualifying activities" }),
        ...activities,
      ]),
      element("button", { type: "submit", text: "Save goal" }),
      status,
    ]);
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      const values = new FormData(form);
      const enabledActivities = values.getAll("enabledActivities");
      if (!enabledActivities.length) {
        status.textContent = "Choose at least one qualifying activity.";
        status.setAttribute("role", "alert");
        return;
      }
      const button = form.querySelector("button");
      button.disabled = true;
      try {
        await mutateJSON(
          `/api/collections/${encodeURIComponent(collection.id)}/progress`,
          {
            target: Number(values.get("target")),
            timezone: values.get("timezone"),
            enabledActivities,
          },
        );
        status.textContent = "Goal saved.";
        status.setAttribute("role", "status");
      } catch (error) {
        status.textContent = error.message;
        status.setAttribute("role", "alert");
      } finally {
        button.disabled = false;
      }
    });
    return element("section", { class: "settings-card" }, [
      element("h2", { text: collection.name }),
      element("p", {
        class: "meta",
        text: "Private to your GitHub identity in this collection.",
      }),
      form,
    ]);
  });
  app.replaceChildren(
    element("section", { class: "page-heading" }, [
      element("p", { class: "eyebrow", text: "Personal settings" }),
      element("h1", { text: "Daily progress goals" }),
      element("p", {
        class: "lede",
        text: "Set a private target, qualifying activities, and timezone for each collection.",
      }),
    ]),
    ...(cards.length
      ? cards
      : [element("p", { class: "empty", text: "You have no authorized collections." })]),
  );
}

function operationalCard(title, state, details) {
  return element("article", { class: "operations-card" }, [
    element("p", { class: "eyebrow", text: title }),
    element("strong", { class: `operations-state operations-${state}`, text: state }),
    element("p", { class: "meta", text: details }),
  ]);
}

function openAdminRefreshDialog() {
  const dialog = element("dialog", {
    class: "admin-refresh-dialog",
    "aria-labelledby": "admin-refresh-dialog-title",
  });
  const status = element("p", { class: "admin-refresh-status" });
  const submit = element("button", { type: "submit", text: "Request refresh" });
  const cancel = element("button", {
    type: "button", class: "secondary", text: "Cancel",
  });
  const form = element("form", { class: "admin-refresh-form" }, [
    element("h2", {
      id: "admin-refresh-dialog-title",
      text: "Refresh all repositories?",
    }),
    element("p", {
      text: "This requests a forced refresh for every configured repository. Already queued or running work will be coalesced and upgraded to a forced refresh.",
    }),
    element("div", { class: "admin-refresh-actions" }, [submit, cancel]),
    status,
  ]);
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    submit.disabled = true;
    cancel.disabled = true;
    status.textContent = "Requesting refresh…";
    status.setAttribute("role", "status");
    try {
      const result = await mutateJSON("/api/admin/refresh", {}, "POST");
      const repositoryLabel = result.repositories === 1 ? "repository" : "repositories";
      status.textContent = `Refresh requested for ${result.repositories} ${repositoryLabel}: ` +
        `${result.enqueued} enqueued, ${result.coalesced} coalesced.`;
      submit.textContent = "Requested";
      cancel.textContent = "Close";
      cancel.disabled = false;
      renderAdmin().catch(() => {});
    } catch (error) {
      submit.disabled = false;
      cancel.disabled = false;
      status.textContent = error.message;
      status.setAttribute("role", "alert");
    }
  });
  cancel.addEventListener("click", () => dialog.close());
  dialog.append(form);
  dialog.addEventListener("close", () => dialog.remove(), { once: true });
  document.body.append(dialog);
  dialog.showModal();
  submit.focus();
}

async function renderAdmin() {
  if (!viewer.authenticated || !viewer.deploymentAdmin) {
    throw new Error("Deployment administrator access is required.");
  }
  const status = await loadJSON("/api/admin/status");
  const collectionOldest = status.collectionJobs.oldestQueuedAt
    ? new Date(status.collectionJobs.oldestQueuedAt).toLocaleString()
    : "none";
  const repositoryRefreshes = status.collectionJobs.repositories || [];
  const lastCacheHits = repositoryRefreshes.reduce(
    (total, repository) => total + repository.lastCacheHits, 0,
  );
  const lastCacheMisses = repositoryRefreshes.reduce(
    (total, repository) => total + repository.lastCacheMisses, 0,
  );
  const lastCacheBypasses = repositoryRefreshes.reduce(
    (total, repository) => total + repository.lastCacheBypasses, 0,
  );
  const cacheLookups = lastCacheHits + lastCacheMisses;
  const cacheHitRate = cacheLookups
    ? `${Math.round((lastCacheHits / cacheLookups) * 100)}%`
    : "n/a";
  const lastForced = repositoryRefreshes.filter((repository) => repository.lastForced).length;
  const analysisOperations = presentAnalysisOperations(status);
  const schedules = presentSchedules(status.schedules);
  const scheduleItems = schedules.length
    ? schedules.map((schedule) => element("li", {}, [
        element("strong", { text: schedule.name }),
        element("code", { text: ` ${schedule.expression}` }),
        element("time", {
          class: "meta",
          datetime: schedule.nextRun,
          text: ` — next in ${schedule.nextLabel}`,
          title: new Date(schedule.nextRun).toLocaleString(),
        }),
      ]))
    : [element("li", { class: "meta", text: "No periodic API calls are enabled." })];
  const correlationJobs = status.correlationJobs || {};
  const correlationState = correlationJobs.failures
    ? "degraded"
    : correlationJobs.running
      ? "running"
      : "healthy";
  const correlationDetails = `${correlationJobs.queued || 0} queued, ` +
    `${correlationJobs.running || 0} running, ${correlationJobs.failures || 0} failed collections. ` +
    (correlationJobs.lastResultAt
      ? `Last success ${new Date(correlationJobs.lastResultAt).toLocaleString()}.`
      : "No successful run recorded.");
  const rate = status.githubRateLimit.state === "observed"
    ? `${status.githubRateLimit.remaining} of ${status.githubRateLimit.limit} remaining`
    : "No GitHub rate-limit response has been observed.";
  const retry = status.githubRetry.state === "none"
    ? "No bounded rate-limit retry has been recorded."
    : `Last ${status.githubRetry.reason} rate-limit event: ${status.githubRetry.state}, retry ${status.githubRetry.retry}, wait ${status.githubRetry.waitSeconds} seconds.`;
  const providers = status.modelProviders.length
    ? status.modelProviders.map((provider) => element("li", {}, [
        element("strong", { text: provider.name }),
        element("span", {
          class: "meta",
          text: ` — ${provider.state}; ${provider.failures} recorded failures`,
        }),
      ]))
    : [element("li", { class: "meta", text: "No model provider is configured." })];
  const failures = status.recentFailures.length
    ? status.recentFailures.map((failure) => element("li", {}, [
        element("strong", { text: `${failure.component}: ${failure.subject}` }),
        element("span", { text: ` — ${failure.message}` }),
      ]))
    : [element("li", { class: "meta", text: "No recent failures." })];
  const audit = status.auditEvents.length
    ? status.auditEvents.map((event) => element("li", {}, [
        element("time", {
          datetime: event.occurredAt,
          text: new Date(event.occurredAt).toLocaleString(),
        }),
        element("span", { text: ` — ${event.event}${event.detail ? ` (${event.detail})` : ""}` }),
      ]))
    : [element("li", { class: "meta", text: "No audit events." })];
  const githubRoutes = presentGitHubRoutes(status.githubRoutes);
  const routeItems = githubRoutes.length
    ? githubRoutes.map((route) => element("li", { class: "github-route" }, [
        element("div", { class: "github-route-heading" }, [
          element("strong", { text: route.repository }),
          element("span", { text: ` — ${route.operationLabel}` }),
          ...(route.pending ? [element("span", { class: "meta", text: " — pending" })] : []),
          ...(route.lastAttempt ? [element("time", {
            class: "meta",
            datetime: route.lastAttempt,
            text: ` — last attempt ${new Date(route.lastAttempt).toLocaleString()}`,
          })] : []),
        ]),
        ...(route.attempts.length
          ? [element("ol", { class: "github-route-attempts" }, route.attempts.map((attempt) =>
              element("li", {}, [
                element("strong", {
                  text: `${attempt.profile} (${attempt.profileTypeLabel}, priority ${attempt.priority})`,
                }),
                element("span", {
                  text: ` — ${attempt.selectedLabel}; ${attempt.outcomeLabel}; failure: ${attempt.failureLabel}`,
                }),
                ...(attempt.attemptedAt ? [element("time", {
                  datetime: attempt.attemptedAt,
                  text: `; ${new Date(attempt.attemptedAt).toLocaleString()}`,
                })] : []),
                ...(attempt.detail ? [element("small", { text: `Detail: ${attempt.detail}` })] : []),
                element("small", { text: `Remediation: ${attempt.remediation}` }),
              ])))
            ]
          : [element("p", { class: "meta", text: "No route attempts recorded." })]),
      ]))
    : [element("li", { class: "meta", text: "No GitHub route attempts recorded." })];
  app.replaceChildren(
    element("section", { class: "page-heading admin-heading" }, [
      element("div", {}, [
        element("p", { class: "eyebrow", text: "Deployment administration" }),
        element("h1", { text: "Operations status" }),
        element("p", {
          class: "lede",
          text: "Non-sensitive runtime health, queues, retention dependencies, and audit activity.",
        }),
      ]),
      element("button", {
        type: "button",
        class: "admin-refresh-button",
        text: "Refresh data",
        title: "Request a forced refresh for every configured repository",
      }),
    ]),
    element("section", { class: "operations-grid" }, [
      operationalCard(
        "Collection queue",
        status.collectionJobs.running ? "running" : "healthy",
        `${status.collectionJobs.queued} queued, ${status.collectionJobs.running} running; oldest ${collectionOldest}. Last complete runs had ${lastCacheHits} cache hits, ${lastCacheMisses} misses, and ${lastCacheBypasses} bypasses (${cacheHitRate} hit rate); ${lastForced} were forced.`,
      ),
      operationalCard(
        "Analysis queue",
        analysisOperations.state,
        analysisOperations.details,
      ),
      operationalCard(
        "Correlation queue",
        correlationState,
        correlationDetails,
      ),
      operationalCard(
        "GitHub rate limit",
        status.githubRetry.state === "failed" ? "degraded" : status.githubRateLimit.state,
        `${rate} ${retry}`,
      ),
      operationalCard(
        "SQLite storage",
        status.storage.state,
        `${humanizeBytes(status.storage.databaseBytes)} on disk.`,
      ),
      operationalCard(
        "Telemetry export",
        status.telemetry.state,
        status.telemetry.message || (status.telemetry.configured
          ? "Configured through OTEL_CONFIG_FILE."
          : "No exporter is configured; telemetry is not exported."),
      ),
      operationalCard(
        "Policy versions",
        "configured",
        `${status.policyVersions.analysisSchema}; ${status.policyVersions.analysisPrompt}; ` +
          `${status.policyVersions.correlationSchema}; ` +
          `${status.policyVersions.correlationPrompt}; ${status.policyVersions.inputFingerprint}.`,
      ),
    ]),
    element("section", { class: "operations-section" }, [
      element("h2", { text: "Schedules (UTC)" }),
      element("ul", {}, scheduleItems),
    ]),
    element("section", { class: "operations-section" }, [
      element("h2", { text: "Model providers" }),
      element("ul", {}, providers),
    ]),
    element("section", { class: "operations-section" }, [
      element("h2", { text: "GitHub routes" }),
      element("ul", { class: "github-routes" }, routeItems),
    ]),
    element("section", { class: "operations-section" }, [
      element("h2", { text: "Recent failures" }),
      element("ul", {}, failures),
    ]),
    element("section", { class: "operations-section" }, [
      element("h2", { text: "Audit events" }),
      element("ul", {}, audit),
    ]),
  );
  app.querySelector(".admin-refresh-button").addEventListener(
    "click", openAdminRefreshDialog,
  );
}

function svgElement(name, attributes = {}, children = []) {
  const node = document.createElementNS("http://www.w3.org/2000/svg", name);
  Object.entries(attributes).forEach(([key, value]) => {
    if (value !== null && value !== undefined) {
      node.setAttribute(key, String(value));
    }
  });
  children.forEach((child) => node.append(child));
  return node;
}

function clippedEdge(from, to) {
  const dx = to.x - from.x;
  const dy = to.y - from.y;
  if (dx === 0 && dy === 0) {
    return { x1: from.x, y1: from.y, x2: to.x, y2: to.y };
  }
  const inset = Math.min(
    dx === 0 ? Infinity : 110 / Math.abs(dx),
    dy === 0 ? Infinity : 38 / Math.abs(dy),
  );
  return {
    x1: from.x + dx * inset,
    y1: from.y + dy * inset,
    x2: to.x - dx * inset,
    y2: to.y - dy * inset,
  };
}

function correlationGraph(group, showHistorical) {
  const members = visibleMembers(group.members || [], showHistorical);
  const memberIDs = new Set(members.map((member) => member.sourceID));
  const positions = graphLayout(members, 760, 420);
  const marker = svgElement("marker", {
    id: "stack-arrow", viewBox: "0 0 10 10", refX: "9", refY: "5",
    markerWidth: "7", markerHeight: "7", orient: "auto-start-reverse",
  }, [svgElement("path", { d: "M 0 0 L 10 5 L 0 10 z" })]);
  const graph = svgElement("svg", {
    class: "correlation-graph", viewBox: "0 0 760 420",
    role: "img", "aria-label": `Relationship graph for ${group.name}`,
  }, [svgElement("defs", {}, [marker])]);
  const viewport = svgElement("g", { class: "correlation-viewport" });
  graph.append(viewport);
  const edgeViews = [];
  const updateEdge = ({ edge, line, label }) => {
    const from = positions.get(edge.from);
    const to = positions.get(edge.to);
    const coordinates = clippedEdge(from, to);
    Object.entries(coordinates).forEach(([name, value]) => line.setAttribute(name, value));
    label.setAttribute(
      "transform",
      `translate(${(from.x + to.x) / 2} ${(from.y + to.y) / 2})`,
    );
  };
  for (const edge of group.edges || []) {
    if (!memberIDs.has(edge.from) || !memberIDs.has(edge.to)) {
      continue;
    }
    const presentation = edgePresentation(edge.type);
    const line = svgElement("line", {
      class: `correlation-edge correlation-edge-${edge.type}`,
      "marker-end": presentation.directed ? "url(#stack-arrow)" : null,
    });
    line.append(svgElement("title", {}, [
      document.createTextNode(`${presentation.label}: ${edge.reason}`),
    ]));
    viewport.append(line);
    const labelWidth = Math.max(54, presentation.label.length * 6.7 + 16);
    const edgeLabel = svgElement("g", {
      class: `correlation-edge-label edge-label-${edge.type}`,
    }, [
      svgElement("rect", {
        x: -labelWidth / 2, y: -11,
        width: labelWidth, height: 22, rx: 7,
      }),
      svgElement("text", {
        x: 0, y: 1,
        "text-anchor": "middle", "dominant-baseline": "middle",
      }, [document.createTextNode(
        presentation.directed ? `${presentation.label} →` : presentation.label,
      )]),
    ]);
    viewport.append(edgeLabel);
    const edgeView = { edge, line, label: edgeLabel };
    edgeViews.push(edgeView);
    updateEdge(edgeView);
  }
  const memberNames = Object.fromEntries((group.members || []).map((member) => [
    member.sourceID, `${member.repository}#${member.number}`,
  ]));
  const graphPoint = (event) => {
    const point = graph.createSVGPoint();
    point.x = event.clientX;
    point.y = event.clientY;
    const matrix = graph.getScreenCTM();
    return matrix ? point.matrixTransform(matrix.inverse()) : point;
  };
  for (const member of members) {
    const position = positions.get(member.sourceID);
    const label = `${member.repository}#${member.number}`;
    const labelLines = memberLabelLines(member);
    const node = svgElement("g", {
      class: `correlation-node correlation-node-${member.state}`,
      transform: `translate(${position.x} ${position.y})`,
    }, [
      svgElement("rect", { x: "-105", y: "-32", width: "210", height: "64", rx: "9" }),
      svgElement("text", {
        x: "0", y: "-7", "text-anchor": "middle",
      }, [
        svgElement("tspan", { x: "0", dy: "0" }, [document.createTextNode(labelLines[0])]),
        svgElement("tspan", { x: "0", dy: "18" }, [document.createTextNode(labelLines[1])]),
      ]),
      svgElement("title", {}, [document.createTextNode([
        `${label}: ${member.title}`,
        ...memberRelationshipSummaries(member.sourceID, group.edges || [], memberNames),
      ].join("\n"))]),
    ]);
    let drag = null;
    node.addEventListener("pointerdown", (event) => {
      event.preventDefault();
      const point = graphPoint(event);
      const current = positions.get(member.sourceID);
      drag = {
        pointerID: event.pointerId,
        offsetX: point.x - current.x,
        offsetY: point.y - current.y,
      };
      node.setPointerCapture(event.pointerId);
      node.classList.add("correlation-node-dragging");
    });
    node.addEventListener("pointermove", (event) => {
      if (!drag || drag.pointerID !== event.pointerId) {
        return;
      }
      const point = graphPoint(event);
      const next = clampNodePosition({
        x: point.x - drag.offsetX,
        y: point.y - drag.offsetY,
      }, 760, 420);
      positions.set(member.sourceID, next);
      node.setAttribute("transform", `translate(${next.x} ${next.y})`);
      edgeViews
        .filter(({ edge }) => edge.from === member.sourceID || edge.to === member.sourceID)
        .forEach(updateEdge);
    });
    const finishDrag = (event) => {
      if (!drag || drag.pointerID !== event.pointerId) {
        return;
      }
      drag = null;
      node.classList.remove("correlation-node-dragging");
      if (node.hasPointerCapture(event.pointerId)) {
        node.releasePointerCapture(event.pointerId);
      }
    };
    node.addEventListener("pointerup", finishDrag);
    node.addEventListener("pointercancel", finishDrag);
    viewport.append(node);
  }
  return graph;
}

function correlationGraphPanel(group, showHistorical) {
  const graph = correlationGraph(group, showHistorical);
  const scroll = element("div", { class: "correlation-graph-scroll" }, [graph]);
  const zoomStatus = element("output", {
    class: "graph-zoom-status", "aria-live": "polite", text: "100%",
  });
  let zoom = 1;
  const setZoom = (next) => {
    zoom = clampGraphZoom(next);
    graph.style.width = `${zoom * 100}%`;
    graph.style.minWidth = `${Math.round(620 * zoom)}px`;
    zoomStatus.textContent = `${Math.round(zoom * 100)}%`;
  };
  const zoomButton = (label, description, change) => {
    const button = element("button", {
      type: "button", class: "graph-zoom-button",
      text: label, "aria-label": description, title: description,
    });
    button.addEventListener("click", () => setZoom(change(zoom)));
    return button;
  };
  scroll.addEventListener("wheel", (event) => {
    if (!event.ctrlKey && !event.metaKey) {
      return;
    }
    event.preventDefault();
    setZoom(zoom + (event.deltaY < 0 ? 0.1 : -0.1));
  }, { passive: false });
  const toolbar = element("div", { class: "correlation-graph-toolbar" }, [
    element("div", { class: "graph-status-legend", "aria-label": "Pull request status colors" }, [
      element("span", { class: "graph-status graph-status-open", text: "Open" }),
      element("span", { class: "graph-status graph-status-closed", text: "Closed" }),
      element("span", { class: "graph-status graph-status-merged", text: "Merged" }),
      element("span", { class: "graph-direction-legend", text: "→ directed relationship" }),
    ]),
    element("div", { class: "graph-zoom-controls" }, [
      zoomButton("−", "Zoom out", (value) => value - 0.2),
      zoomStatus,
      zoomButton("+", "Zoom in", (value) => value + 0.2),
      zoomButton("Reset", "Reset zoom", () => 1),
    ]),
  ]);
  return [toolbar, scroll];
}

function correlationCloseButton() {
  return element("button", {
    type: "button",
    class: "drawer-close correlation-close-button",
    text: "×",
    "aria-label": "Close feature group",
    title: "Close",
  });
}

async function openCorrelationGroup(collectionID, summary, trigger) {
  const dialog = element("dialog", {
    class: "correlation-dialog", "aria-labelledby": "correlation-dialog-title",
  }, [element("p", { text: "Loading feature relationships…" })]);
  document.body.append(dialog);
  dialog.addEventListener("click", (event) => {
    if (event.target === dialog && isPointOutsideRect(
      { x: event.clientX, y: event.clientY },
      dialog.getBoundingClientRect(),
    )) {
      dialog.close();
    }
  });
  dialog.addEventListener("close", () => {
    dialog.remove();
    trigger.focus();
  }, { once: true });
  dialog.showModal();
  try {
    const group = await loadJSON(groupDetailPath(collectionID, summary.id));
    const historical = (group.members || []).filter((member) => member.state !== "open");
    const graphContainer = element("div", { class: "correlation-graph-frame" });
    const members = element("ul", { class: "correlation-members" });
    const renderMembers = (showHistorical) => {
      graphContainer.replaceChildren(...correlationGraphPanel(group, showHistorical));
      members.replaceChildren(...visibleMembers(group.members || [], showHistorical).map((member) =>
        element("li", {}, [
          safeGitHubURL(member.url)
            ? element("a", {
                href: safeGitHubURL(member.url), target: "_blank", rel: "noopener noreferrer",
                text: `${member.repository}#${member.number}`,
              })
            : element("strong", { text: `${member.repository}#${member.number}` }),
          ` — ${member.title} (${member.state})`,
        ])));
    };
    const disclosure = historical.length
      ? element("label", { class: "correlation-history-toggle" }, [
          element("input", { type: "checkbox" }),
          ` Show ${historical.length} closed or merged member${historical.length === 1 ? "" : "s"}`,
        ])
      : null;
    disclosure?.querySelector("input").addEventListener("change", (event) => {
      renderMembers(event.target.checked);
    });
    const edges = (group.edges || []).map((edge) => element("li", {}, [
      element("code", { text: `${edge.from} ${edge.type} ${edge.to}` }),
      element("span", {
        text: ` — ${edge.reason} (${edge.confidence} confidence; evidence: ${edge.sourceIDs.join(", ")})`,
      }),
    ]));
    dialog.replaceChildren(
      element("div", { class: "drawer-header" }, [
        element("div", {}, [
          element("p", { class: "eyebrow", text: "Model-inferred feature group" }),
          element("h2", { id: "correlation-dialog-title", text: group.name }),
        ]),
        correlationCloseButton(),
      ]),
      element("p", { class: "lede", text: group.description }),
      graphContainer,
      ...(disclosure ? [disclosure] : []),
      element("section", { class: "drawer-section" }, [
        element("h3", { text: "Pull requests" }), members,
      ]),
      element("section", { class: "drawer-section" }, [
        element("h3", { text: "Typed relationships" }),
        element("ul", {}, edges.length ? edges : [element("li", { text: "No direct edges were inferred." })]),
      ]),
      element("details", { class: "correlation-text" }, [
        element("summary", { text: "Text representation" }),
        element("pre", { text: group.textGraph }),
      ]),
      element("p", {
        class: "meta",
        text: `Model-inferred with ${group.confidence} confidence by ${group.provenance.provider}/${group.provenance.model} ` +
          `at ${new Date(group.provenance.correlatedAt).toLocaleString()}; ` +
          `${group.provenance.promptVersion}, ${group.provenance.schemaVersion}.`,
      }),
    );
    renderMembers(false);
    dialog.querySelector(".drawer-close").addEventListener("click", () => dialog.close());
    dialog.querySelector(".drawer-close").focus();
  } catch (error) {
    dialog.replaceChildren(
      element("h2", { id: "correlation-dialog-title", text: "Unable to load group" }),
      element("p", { role: "alert", text: error.message }),
      correlationCloseButton(),
    );
    dialog.querySelector(".drawer-close").addEventListener("click", () => dialog.close());
  }
}

async function renderGroups(collectionID, query) {
  const [data, collectionsData] = await Promise.all([
    loadJSON(`/api/collections/${encodeURIComponent(collectionID)}/groups`),
    loadJSON("/api/collections"),
  ]);
  const collection = (collectionsData.collections || []).find((item) => item.id === collectionID);
  if (!collection) {
    throw new Error("Collection not found.");
  }
  const rows = (data.groups || []).map((group) => {
    const row = element("tr", {
      class: "clickable-row", tabindex: "0", "aria-haspopup": "dialog",
      "aria-label": `Open relationship graph for ${group.name}`,
    }, [
      element("td", {}, [
        element("strong", { text: group.name }),
        element("small", {
          class: "group-provenance",
          text: `Model-inferred · ${group.confidence} · ${group.provider}/${group.model} · ` +
            new Date(group.correlatedAt).toLocaleString(),
        }),
      ]),
      element("td", { class: "group-description", text: group.description }),
      element("td", { text: String(group.openMemberCount) }),
    ]);
    const activate = (event) => {
      if (!isRowActivation(event)) {
        return;
      }
      if (event.type === "keydown") {
        event.preventDefault();
      }
      openCorrelationGroup(collectionID, group, row);
    };
    row.addEventListener("click", activate);
    row.addEventListener("keydown", activate);
    return row;
  });
  const status = data.status || { state: "never" };
  const statusText = status.state === "failed"
    ? `The latest correlation attempt failed. Last successful groups remain visible.${status.lastSuccessAt ? ` Last success: ${new Date(status.lastSuccessAt).toLocaleString()}.` : ""}`
    : status.lastSuccessAt
      ? `Groups last updated ${new Date(status.lastSuccessAt).toLocaleString()}.`
      : "No successful correlation has completed.";
  const routeWarnings = routeWarningsControl(collection);
  renderCollectionSwitcher(collectionID, collectionsData.collections || []);
  app.replaceChildren(element("div", { class: "collection-layout" }, [
    collectionSidebar(collectionID, query, authorizedFor(collectionID)),
    element("div", { class: "collection-content" }, [
      element("section", { class: "collection-summary" }, [
        element("p", { class: "eyebrow", text: "Model-inferred feature groups" }),
        element("h1", { text: `${collection.name} groups` }),
        element("p", { class: status.state === "failed" ? "context-warning" : "meta", text: statusText }),
        ...(routeWarnings ? [routeWarnings] : []),
      ]),
      element("div", { class: "table-frame" }, [
        element("table", { class: "groups-table" }, [
          element("thead", {}, [element("tr", {}, [
            element("th", { scope: "col", text: "Group" }),
            element("th", { scope: "col", text: "Description" }),
            element("th", { scope: "col", text: "Open PRs" }),
          ])]),
          element("tbody", {}, rows.length ? rows : [element("tr", {}, [
            element("td", {
              colspan: "3", class: "empty",
              text: "No correlated feature groups are available.",
            }),
          ])]),
        ]),
      ]),
    ]),
  ]));
}

async function renderCollection(collectionID) {
  const path = `/api/collections/${encodeURIComponent(collectionID)}/pull-requests`;
  const query = new URLSearchParams(window.location.search);
  if (query.get("view") === "groups") {
    await renderGroups(collectionID, query);
    return;
  }
  const today = query.get("view") === "today";
  const apiQuery = new URLSearchParams(query);
  if (today) {
    apiQuery.delete("view");
  }
  const sorting = parseSorting(query);
  const [data, collectionsData] = await Promise.all([
    loadJSON(`${path}${apiQuery.size ? `?${apiQuery}` : ""}`),
    loadJSON("/api/collections"),
  ]);
  const showImportant = authorizedFor(collectionID);
  const progress = showImportant
    ? await loadProgress(collectionID)
    : null;
  const repositories = [...new Set(data.collection.repositories)].sort();
  const tableLayout = loadTableLayout(showImportant);
  const columns = visibleTableColumns(tableLayout);
  const sortableColumns = new Set([
    "number", "repository", "title", "churn", "quality", "review_load", "review", "updated",
  ]);
  const headers = columns.map((column) => ({
    ...column,
    key: sortableColumns.has(column.id) ? column.id : null,
    class: column.id === "title"
      ? "pr-title-heading"
      : column.id === "review_load"
          ? "review-load-heading"
        : column.id === "actions"
          ? "actions-heading"
          : null,
  }));
  const applySorting = (key) => {
    const next = advanceSorting(sorting, key);
    const nextQuery = new URLSearchParams(window.location.search);
    writeSorting(nextQuery, next);
    const encoded = nextQuery.toString();
    window.history.pushState({}, "", `${window.location.pathname}${encoded ? `?${encoded}` : ""}`);
    render();
  };
  const tableFor = (pullRequests, emptyMessage) => {
    const rows = pullRequests.map((pr) => {
      const row = prRow(collectionID, pr, columns);
      const activate = (event) => {
        if (!isRowActivation(event)) {
          return;
        }
        if (event.type === "keydown") {
          event.preventDefault();
        }
        const next = new URLSearchParams(window.location.search);
        next.set("pr", `${pr.repository}#${pr.number}`);
        window.history.pushState({}, "", `${window.location.pathname}?${next}`);
        openPullRequestDetail(collectionID, pr, row);
      };
      row.addEventListener("click", activate);
      row.addEventListener("keydown", activate);
      return row;
    });
    return element("table", {}, [
      element("thead", {}, [
        element("tr", {}, headers.map((header) => {
          if (!header.key) {
            return element("th", { scope: "col", class: header.class, text: header.label });
          }
          const cell = sortHeader(header, sorting);
          if (header.class) {
            cell.classList.add(header.class);
          }
          cell.querySelector("button").addEventListener("click", () => applySorting(header.key));
          return cell;
        })),
      ]),
      element("tbody", {}, rows.length
        ? rows
        : [element("tr", {}, [
            element("td", {
              colspan: String(columns.length),
              class: "empty",
              text: emptyMessage,
            }),
          ])]),
    ]);
  };
  const hiddenTableFor = (pullRequests, emptyMessage) => {
    const rows = pullRequests.map((pr) => {
      const view = presentPullRequest(pr);
      const hiddenState = presentHidden(pr.personal?.hidden);
      const githubURL = safeGitHubURL(pr.url);
      const restore = element("button", {
        type: "button",
        class: "secondary restore-row-button",
        text: "Restore",
        "aria-label": `Restore ${pr.title}`,
      });
      restore.addEventListener("click", async (event) => {
        event.stopPropagation();
        restore.disabled = true;
        try {
          await mutate(hiddenPath(collectionID, pr), "DELETE");
          render();
        } catch (error) {
          restore.disabled = false;
          restore.title = error.message;
        }
      });
      const row = element("tr", {
        class: "clickable-row",
        tabindex: "0",
        "aria-haspopup": "dialog",
        "aria-label": `Open details for ${view.repository} ${view.number}: ${view.title}`,
      }, [
        element("td", {}, [
          githubURL
            ? element("a", {
                href: githubURL, target: "_blank", rel: "noreferrer",
                text: `${view.repository}${view.number}`,
              })
            : element("span", { text: `${view.repository}${view.number}` }),
        ]),
        element("td", { text: view.title }),
        element("td", {}, [
          element("strong", {
            text: hiddenState.kind === "ignored"
              ? "Until restored"
              : hiddenState.condition || "Until next activity",
            title: pr.personal?.hidden?.snoozedUntil
              ? new Date(pr.personal.hidden.snoozedUntil).toLocaleString()
              : null,
          }),
          ...(hiddenState.newActivity ? [
            element("span", { class: "new-activity-badge", text: "New activity" }),
          ] : []),
        ]),
        element("td", {
          text: hiddenState.reason || "—",
          class: hiddenState.reason ? "" : "meta",
        }),
        element("td", {}, [restore]),
      ]);
      const activate = (event) => {
        if (!isRowActivation(event)) {
          return;
        }
        if (event.type === "keydown") {
          event.preventDefault();
        }
        const next = new URLSearchParams(window.location.search);
        next.set("pr", `${pr.repository}#${pr.number}`);
        window.history.pushState({}, "", `${window.location.pathname}?${next}`);
        openPullRequestDetail(collectionID, pr, row);
      };
      row.addEventListener("click", activate);
      row.addEventListener("keydown", activate);
      return row;
    });
    return element("table", { class: "hidden-table" }, [
      element("thead", {}, [
        element("tr", {}, ["Pull request", "Title", "Hidden until", "Notes", "Action"]
          .map((text) => element("th", { scope: "col", text }))),
      ]),
      element("tbody", {}, rows.length
        ? rows
        : [element("tr", {}, [
            element("td", { colspan: "5", class: "empty", text: emptyMessage }),
          ])]),
    ]);
  };
  const mine = query.get("view") === "mine"
    ? groupMinePullRequests(data.pullRequests, viewer.id)
    : null;
  const hidden = query.get("view") === "hidden"
    ? groupHiddenPullRequests(data.pullRequests)
    : null;
  const tableSections = mine
    ? [
        ["Authored by me", mine.authored, "No authored pull requests match."],
        ["Important", mine.important, "No Important pull requests match."],
      ].map(([title, pullRequests, emptyMessage]) =>
        element("section", { class: "mine-table-section" }, [
          element("h2", { text: title }),
          element("div", { class: "table-frame" }, [
            tableFor(pullRequests, emptyMessage),
          ]),
        ]))
    : hidden
      ? [
          ["Snoozed", hidden.snoozed, "No snoozed pull requests match."],
          ["Ignored", hidden.ignored, "No ignored pull requests match."],
        ].map(([title, pullRequests, emptyMessage]) =>
          element("section", { class: "mine-table-section" }, [
            element("h2", { text: title }),
            element("div", { class: "table-frame" }, [
              hiddenTableFor(pullRequests, emptyMessage),
            ]),
          ]))
    : [
        element("div", { class: "table-frame" }, [
          tableFor(data.pullRequests, "No pull requests have been collected yet."),
        ]),
      ];
  renderCollectionSwitcher(collectionID, collectionsData.collections || []);
  const routeWarnings = routeWarningsControl(data.collection);
  const content = element("div", { class: "collection-content" }, [
    element("section", { class: "collection-summary" }, [
      element("h1", { class: "sr-only", text: data.collection.name }),
      element("p", {
        class: "lede",
        text: data.collection.description || "No description provided.",
      }),
      refreshStatus(data.collection),
      ...(routeWarnings ? [routeWarnings] : []),
    ]),
    ...(today
      ? [todayView(progress)]
      : [
          ...(!hidden ? [listControls(
            query,
            repositories,
            data.counts.matched,
            modifyTableControl(tableLayout, render),
          )] : []),
          ...tableSections,
        ]),
  ]);
  renderTodayProgress(collectionID, progress, today);
  app.replaceChildren(
    element("div", {
      class: "collection-layout",
    }, [
      collectionSidebar(collectionID, query, showImportant),
      content,
    ]),
    ...(!today && data.page.nextCursor ? [
      element("div", { class: "pagination" }, [
        element("button", { type: "button", text: "Next page", class: "next-page" }),
      ]),
    ] : []),
  );
  const directIdentity = query.get("pr")?.match(/^([^/]+)\/([^#]+)#([1-9][0-9]*)$/);
  if (directIdentity && !document.querySelector(".detail-drawer")) {
    const trigger = element("button", {
      type: "button", class: "direct-detail-trigger", hidden: "",
    });
    app.append(trigger);
    openPullRequestDetail(collectionID, {
      repository: `${directIdentity[1]}/${directIdentity[2]}`,
      number: Number(directIdentity[3]),
    }, trigger);
  }
  app.querySelector(".next-page")?.addEventListener("click", () => {
    const next = new URLSearchParams(window.location.search);
    next.set("cursor", data.page.nextCursor);
    window.history.pushState({}, "", `${window.location.pathname}?${next}`);
    render();
  });
  if (filterFocus) {
    const control = app.querySelector(`[name="${filterFocus.name}"]`);
    control?.focus();
    control?.setSelectionRange(filterFocus.start, filterFocus.end);
    filterFocus = null;
  }
  if (tablePickerFocus) {
    let control = app.querySelector(".restore-table-defaults");
    if (tablePickerFocus.id) {
      const row = app.querySelector(
        `.table-column-item[data-column-id="${tablePickerFocus.id}"]`,
      );
      control = tablePickerFocus.control === "checkbox"
        ? row?.querySelector('input[type="checkbox"]')
        : row?.querySelector(`[data-direction="${tablePickerFocus.control}"]`);
      if (control?.disabled) {
        control = row?.querySelector('input[type="checkbox"]');
      }
    }
    control?.focus();
    tablePickerFocus = null;
  }
}

async function render() {
  app.setAttribute("aria-busy", "true");
  collectionControls.replaceChildren();
  contextControls.replaceChildren();
  const location = parseLocation(window.location.pathname);
  try {
    if (!location) {
      throw new Error("This page does not exist.");
    }
    if (location.page === "collections") {
      await renderCollections();
    } else if (location.page === "settings") {
      await renderSettings();
    } else if (location.page === "admin") {
      await renderAdmin();
    } else if (location.page === "collection") {
      await renderCollection(location.collectionID);
    } else {
      await renderAuthor(location.collectionID, location.login);
    }
  } catch (error) {
    app.replaceChildren(
      element("section", { class: "error", role: "alert" }, [
        element("h1", { text: "Unable to load Maintainer Cockpit" }),
        element("p", { text: error.message }),
      ]),
    );
  } finally {
    app.setAttribute("aria-busy", "false");
  }
}

document.addEventListener("click", navigate);
document.addEventListener("click", (event) => {
  const switcher = collectionControls.querySelector(".collection-switcher[open]");
  if (switcher && !switcher.contains(event.target)) {
    switcher.open = false;
  }
});
window.addEventListener("popstate", render);
loadJSON("/api/viewer")
  .then((loadedViewer) => {
    viewer = loadedViewer;
    renderViewer();
    render();
  })
  .catch(renderViewer);
render();
