// The poll's page. Vite bundles this file, and style.css, into dist/.

import "./style.css";

const list = document.querySelector("#options");
const status = document.querySelector("#status");
let myVote = remembered();

function remembered() {
  try {
    return localStorage.getItem("poll-vote");
  } catch {
    return null;
  }
}

function remember(option) {
  myVote = option;
  try {
    localStorage.setItem("poll-vote", option);
  } catch {
    // Private windows may refuse; the vote still counts.
  }
}

async function request(method, path, body) {
  const response = await fetch(path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  const data = await response.json();
  if (!response.ok) throw new Error(data.error || response.statusText);
  return data;
}

function render(results) {
  const total = results.reduce((sum, r) => sum + r.votes, 0);
  status.textContent = myVote
    ? `You voted for ${myVote}. ${total} ${total === 1 ? "vote" : "votes"} so far.`
    : "Pick one. The results update live.";
  list.replaceChildren(
    ...results.map(({ option, votes }) => {
      const share = total ? Math.round((votes / total) * 100) : 0;
      const li = document.createElement("li");
      const button = document.createElement("button");
      button.type = "button";
      button.className = option === myVote ? "mine" : "";
      button.style.setProperty("--share", `${share}%`);
      button.addEventListener("click", () => vote(option));

      const name = document.createElement("span");
      name.textContent = option;
      const count = document.createElement("span");
      count.className = "count";
      count.textContent = total ? `${share}% · ${votes}` : "";
      button.append(name, count);
      li.append(button);
      return li;
    }),
  );
}

function showError(error) {
  const box = document.querySelector("#error");
  box.textContent = error ? error.message : "";
  box.hidden = !error;
}

async function vote(option) {
  try {
    const { results } = await request("POST", "/api/votes", { option });
    remember(option);
    render(results);
    showError(null);
  } catch (error) {
    showError(error);
  }
}

async function refresh() {
  try {
    const poll = await request("GET", "/api/poll");
    document.title = poll.question;
    document.querySelector("#question").textContent = poll.question;
    document.querySelector("#release").textContent = `Release ${poll.release}`;
    render(poll.results);
    showError(null);
  } catch (error) {
    showError(error);
  }
}

refresh();
setInterval(refresh, 3000);
