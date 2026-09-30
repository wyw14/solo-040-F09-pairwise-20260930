const { api, shortID, setMessage, withBusy } = window.LockFlow;
const form = document.querySelector("#plan-form");
const message = document.querySelector("#plan-message");

const toLocalInput = (value) => {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  const pad = (n) => String(n).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
};

const arrivalLabel = (plan) => plan.eta
  ? `ETA ${new Date(plan.eta).toLocaleString()}`
  : `FCFS ${new Date(plan.created_at).toLocaleTimeString()}`;

async function refresh() {
  const [plans, chambers, health] = await Promise.all([api("/api/plans"), api("/api/chambers"), api("/healthz")]);
  document.querySelector("#health").textContent = health.status === "ok" ? "Devices online" : "Attention required";
  const rows = Object.entries(plans).flatMap(([chamber, items]) => items.map(({ plan, queue_position: position, released }) => {
    const status = released
      ? '<span class="status">Released · locked</span>'
      : '<span class="muted">Waiting</span>';
    const controls = released
      ? '<span class="muted">Order committed</span>'
      : `<span class="inline-adjust"><input type="number" min="1" value="${plan.priority}" data-plan="${plan.id}" data-field="priority" style="width:5rem">`
        + `<input type="datetime-local" value="${toLocalInput(plan.eta)}" data-plan="${plan.id}" data-field="eta">`
        + `<button type="button" data-apply="${plan.id}">Apply</button></span>`;
    return `<tr><td>${position}</td><td>${chamber}</td><td>${shortID(plan.id)}</td><td>${plan.direction}</td>`
      + `<td>${plan.vessels.length}</td><td>${plan.priority}</td><td>${arrivalLabel(plan)}</td><td>${status}</td><td>${controls}</td></tr>`;
  }));
  document.querySelector("#plans").innerHTML = rows.join("") || '<tr><td colspan="9" class="muted">No active plans</td></tr>';
  const current = chambers.find((item) => item.cycle);
  document.querySelector("#metric-chamber").textContent = current?.chamber_id || "Idle";
  document.querySelector("#metric-stage").textContent = current?.cycle?.stage || "-";
  document.querySelector("#metric-cycle").textContent = shortID(current?.cycle?.id);
}

document.querySelector("#plans").addEventListener("click", async (event) => {
  const planID = event.target?.dataset?.apply;
  if (!planID) return;
  const button = event.target;
  await withBusy(button, async () => {
    try {
      const fields = document.querySelectorAll(`[data-plan="${planID}"]`);
      const body = {};
      fields.forEach((field) => {
        if (field.dataset.field === "priority") body.priority = Number(field.value) || 100;
        if (field.dataset.field === "eta") {
          // Empty value explicitly clears the announced ETA; the queue
          // then falls back to first-come-first-served by creation time.
          body.eta = field.value ? new Date(field.value).toISOString() : "";
        }
      });
      const updated = await api(`/api/plans/${planID}`, { method: "PATCH", body: JSON.stringify(body) });
      setMessage(message, `Plan ${shortID(updated.id)} reordered: priority ${updated.priority}, ${arrivalLabel(updated)}.`);
      await refresh();
    } catch (error) { setMessage(message, error.message); }
  });
});

form.addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = form.querySelector("button");
  await withBusy(button, async () => {
    try {
      const etaValue = document.querySelector("#eta").value;
      const result = await api("/api/plans", { method: "POST", body: JSON.stringify({
        chamber_id: document.querySelector("#chamber").value,
        direction: document.querySelector("#direction").value,
        vessels: [{ name: document.querySelector("#vessel").value, call_sign: document.querySelector("#call-sign").value, length_m: Number(document.querySelector("#length").value) }],
        priority: Number(document.querySelector("#priority").value) || 100,
        eta: etaValue ? new Date(etaValue).toISOString() : "",
      }) });
      setMessage(message, `Plan ${shortID(result.plan.id)} admitted to ${result.plan.chamber_id}.`);
      await refresh();
    } catch (error) { setMessage(message, error.message); }
  });
});

refresh().catch((error) => setMessage(message, error.message));
setInterval(() => refresh().catch(() => {}), 5000);
