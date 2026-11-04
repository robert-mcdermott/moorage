"""Tasks: a small task list with a JSON API and a web page.

    uvicorn tasks.main:app
"""

import os
from contextlib import closing
from pathlib import Path
from typing import Annotated

from fastapi import FastAPI, HTTPException, Response, status
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel, StringConstraints

from .db import connect

Title = Annotated[str, StringConstraints(strip_whitespace=True, min_length=1, max_length=200)]


class NewTask(BaseModel):
    title: Title


class TaskChange(BaseModel):
    title: Title | None = None
    done: bool | None = None


class Task(BaseModel):
    id: int
    title: str
    done: bool
    created_at: str


app = FastAPI(title="Tasks")


@app.get("/healthz")
def health() -> dict:
    # Moorage sends traffic to a new release only once this answers. Reading
    # the table checks that the database is there and migrated.
    with closing(connect()) as conn:
        conn.execute("SELECT 1 FROM tasks LIMIT 1").fetchall()
    return {"status": "ok"}


@app.get("/api/info")
def info() -> dict:
    # Variables Moorage sets for every instance, and one from [env].
    return {
        "title": os.environ.get("TASKS_TITLE", "Tasks"),
        "release": os.environ.get("MOORAGE_RELEASE", "local"),
        "instance": os.environ.get("MOORAGE_INSTANCE", "1"),
    }


@app.get("/api/tasks")
def list_tasks() -> list[Task]:
    with closing(connect()) as conn:
        rows = conn.execute("SELECT * FROM tasks ORDER BY done, created_at DESC, id DESC").fetchall()
    return [Task(**row) for row in rows]


@app.post("/api/tasks", status_code=status.HTTP_201_CREATED)
def add_task(new: NewTask) -> Task:
    with closing(connect()) as conn:
        row = conn.execute("INSERT INTO tasks (title) VALUES (?) RETURNING *", (new.title,)).fetchone()
    return Task(**row)


@app.patch("/api/tasks/{task_id}")
def change_task(task_id: int, change: TaskChange) -> Task:
    with closing(connect()) as conn:
        row = conn.execute(
            "UPDATE tasks SET title = coalesce(?, title), done = coalesce(?, done) WHERE id = ? RETURNING *",
            (change.title, change.done, task_id),
        ).fetchone()
    if row is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "no such task")
    return Task(**row)


@app.delete("/api/tasks/{task_id}", status_code=status.HTTP_204_NO_CONTENT)
def delete_task(task_id: int) -> Response:
    with closing(connect()) as conn:
        if conn.execute("DELETE FROM tasks WHERE id = ?", (task_id,)).rowcount == 0:
            raise HTTPException(status.HTTP_404_NOT_FOUND, "no such task")
    return Response(status_code=status.HTTP_204_NO_CONTENT)


@app.delete("/api/tasks", status_code=status.HTTP_204_NO_CONTENT)
def clear_done() -> Response:
    with closing(connect()) as conn:
        conn.execute("DELETE FROM tasks WHERE done")
    return Response(status_code=status.HTTP_204_NO_CONTENT)


# The web page: index.html, app.js and style.css. Mounted last, so that the
# routes above come first.
app.mount("/", StaticFiles(directory=Path(__file__).parent / "static", html=True), name="static")
