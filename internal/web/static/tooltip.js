const tooltipGap = 7;
const viewportMargin = 12;

export function floatingTooltipPosition(triggerRect, tooltipRect, viewport) {
  const centeredLeft = triggerRect.left + (triggerRect.width - tooltipRect.width) / 2;
  const maxLeft = Math.max(viewportMargin, viewport.width - tooltipRect.width - viewportMargin);
  const left = Math.min(Math.max(centeredLeft, viewportMargin), maxLeft);
  const above = triggerRect.top - tooltipGap - tooltipRect.height;
  const top = above >= viewportMargin
    ? above
    : Math.min(triggerRect.bottom + tooltipGap, viewport.height - tooltipRect.height - viewportMargin);

  return { top, left };
}

export function attachFloatingTooltip(trigger, text) {
  let tooltip = null;
  let hovered = false;
  let focused = false;

  const position = () => {
    if (!tooltip) {
      return;
    }
    const { top, left } = floatingTooltipPosition(
      trigger.getBoundingClientRect(),
      tooltip.getBoundingClientRect(),
      { width: window.innerWidth, height: window.innerHeight },
    );
    tooltip.style.top = `${top}px`;
    tooltip.style.left = `${left}px`;
    tooltip.style.visibility = "visible";
  };

  const show = () => {
    if (tooltip) {
      return;
    }
    tooltip = document.createElement("span");
    tooltip.className = "analysis-tooltip analysis-tooltip-floating";
    tooltip.setAttribute("role", "tooltip");
    tooltip.textContent = text;
    document.body.append(tooltip);
    position();
    window.addEventListener("resize", position);
    document.addEventListener("scroll", position, true);
  };

  const hide = () => {
    if (hovered || focused) {
      return;
    }
    tooltip?.remove();
    tooltip = null;
    window.removeEventListener("resize", position);
    document.removeEventListener("scroll", position, true);
  };

  trigger.addEventListener("pointerenter", () => {
    hovered = true;
    show();
  });
  trigger.addEventListener("pointerleave", () => {
    hovered = false;
    hide();
  });
  trigger.addEventListener("focus", () => {
    focused = true;
    show();
  });
  trigger.addEventListener("blur", () => {
    focused = false;
    hide();
  });
}
