import type { Task } from "../api";

export function TaskList({ tasks }: { tasks: Task[] }) {
  return (
    <ul className="task-list">
      {tasks.map((task) => (
        <li key={task.id} className={task.done ? "done" : "open"}>
          <input type="checkbox" checked={task.done} readOnly />
          {task.title}
        </li>
      ))}
    </ul>
  );
}
