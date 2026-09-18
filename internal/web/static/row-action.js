const interactiveSelector = "a, button, input, select, textarea";

export function isRowActivation(event) {
  if (event.type === "click") {
    return !event.target.closest(interactiveSelector);
  }
  return event.type === "keydown" &&
    event.target === event.currentTarget &&
    (event.key === "Enter" || event.key === " ");
}
