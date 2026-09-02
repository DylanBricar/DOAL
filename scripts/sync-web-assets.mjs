import * as lucideIcons from "lucide";
import { mkdir, writeFile } from "node:fs/promises";

const iconExports = {
  "alert-triangle": "AlertTriangle",
  "bar-chart-2": "BarChart2",
  "check-circle": "CheckCircle",
  clock: "Clock",
  database: "Database",
  folder: "Folder",
  globe: "Globe",
  "hard-drive": "HardDrive",
  inbox: "Inbox",
  info: "Info",
  monitor: "Monitor",
  moon: "Moon",
  pause: "Pause",
  play: "Play",
  power: "Power",
  save: "Save",
  "scroll-text": "ScrollText",
  shield: "Shield",
  sliders: "Sliders",
  sun: "Sun",
  trash: "Trash",
  "trash-2": "Trash2",
  upload: "Upload",
  "upload-cloud": "UploadCloud",
  "x-circle": "XCircle",
  zap: "Zap"
};

const icons = Object.fromEntries(
  Object.entries(iconExports).map(([name, exportName]) => [name, lucideIcons[exportName]])
);
if (Object.values(icons).some(icon => !Array.isArray(icon))) {
  throw new Error("A selected Lucide icon export is missing");
}

function bootstrapLucide(iconSet) {
  const svgNS = "http://www.w3.org/2000/svg";
  function createNode([tag, attributes, children = []]) {
    const node = document.createElementNS(svgNS, tag);
    for (const [name, value] of Object.entries(attributes)) node.setAttribute(name, String(value));
    for (const child of children) node.appendChild(createNode(child));
    return node;
  }
  function createIcons() {
    document.querySelectorAll("i[data-lucide]").forEach(placeholder => {
      const name = placeholder.dataset.lucide;
      const icon = iconSet[name];
      if (!icon) return;
      const svg = createNode(["svg", {
        xmlns: svgNS,
        width: 24,
        height: 24,
        viewBox: "0 0 24 24",
        fill: "none",
        stroke: "currentColor",
        "stroke-width": 2,
        "stroke-linecap": "round",
        "stroke-linejoin": "round",
        class: "lucide lucide-" + name + " " + placeholder.className,
        "aria-hidden": "true",
        "data-lucide": name
      }, icon]);
      placeholder.replaceWith(svg);
    });
  }
  window.lucide = { createIcons };
}

const runtime = "(" + bootstrapLucide.toString() + ")(" + JSON.stringify(icons) + ");\n";
await mkdir("web/static", { recursive: true });
await writeFile("web/static/lucide.min.js", runtime, "utf8");
