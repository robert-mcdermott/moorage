// The task list's page: it talks to the JSON API under /api.

const list = document.querySelector("#tasks");
const form = document.querySelector("#add");
const input = document.querySelector("#new-title");
let tasks = [];
let filter = "all";

async function api(method, path, body) {
  const response = await fetch(path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!response.ok) {
    throw new Error(`${method} ${path}: ${response.status} ${response.statusText}`);
  }
  return response.status === 204 ? null : response.json();
}

function showError(error) {
  const box = document.querySelector("#error");
  box.textContent = error ? `Something went wrong: ${error.message}` : "";
  box.hidden = !error;
}

function render() {
  const shown = tasks.filter((t) => filter === "all" || (filter === "done") === t.done);
  list.replaceChildren(...shown.map(item));
  document.querySelector("#empty").hidden = shown.length > 0;
  const open = tasks.filter((t) => !t.done).length;
  document.querySelector("#summary").textContent =
    tasks.length === 0 ? "" : `${open} open, ${tasks.length - open} done`;
  document.querySelector("#clear").disabled = tasks.length === open;
}

function item(task) {
  const li = document.createElement("li");
  li.className = task.done ? "done" : "";

  const box = document.createElement("input");
  box.type = "checkbox";
  box.checked = task.done;
  box.id = `task-${task.id}`;
  box.addEventListener("change", () => update(task.id, { done: box.checked }));

  const label = document.createElement("label");
  label.htmlFor = box.id;
  label.textContent = task.title;

  const remove = document.createElement("button");
  remove.type = "button";
  remove.className = "quiet";
  remove.textContent = "Delete";
  remove.setAttribute("aria-label", `Delete “${task.title}”`);
  remove.addEventListener("click", () => run(async () => {
    await api("DELETE", `/api/tasks/${task.id}`);
    tasks = tasks.filter((t) => t.id !== task.id);
  }));

  li.append(box, label, remove);
  return li;
}

async function update(id, change) {
  await run(async () => {
    const changed = await api("PATCH", `/api/tasks/${id}`, change);
    tasks = tasks.map((t) => (t.id === id ? changed : t));
  });
}

// run does one change and redraws the list, showing what went wrong if
// anything did.
async function run(change) {
  try {
    await change();
    showError(null);
  } catch (error) {
    showError(error);
  }
  render();
}

form.addEventListener("submit", (event) => {
  event.preventDefault();
  const title = input.value.trim();
  if (!title) return;
  // Cleared now, not once the task is saved, so that typing the next one
  // meanwhile is not lost.
  input.value = "";
  run(async () => {
    try {
      tasks = [await api("POST", "/api/tasks", { title }), ...tasks];
    } catch (error) {
      if (!input.value) input.value = title;
      throw error;
    }
  });
});

document.querySelectorAll("[data-filter]").forEach((button) => {
  button.addEventListener("click", () => {
    filter = button.dataset.filter;
    document.querySelectorAll("[data-filter]").forEach((b) => b.setAttribute("aria-pressed", b === button));
    render();
  });
});

document.querySelector("#clear").addEventListener("click", () => run(async () => {
  await api("DELETE", "/api/tasks");
  tasks = tasks.filter((t) => !t.done);
}));

run(async () => {
  const [info, all] = await Promise.all([api("GET", "/api/info"), api("GET", "/api/tasks")]);
  document.title = info.title;
  document.querySelector("#title").textContent = info.title;
  document.querySelector("#release").textContent = `Release ${info.release}, instance ${info.instance}`;
  tasks = all;
});
