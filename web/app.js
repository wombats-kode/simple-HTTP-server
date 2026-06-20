(() => {
  const formatSize = (bytes) => {
    if (bytes < 1024) return `${bytes} B`;
    const units = ["KiB", "MiB", "GiB", "TiB"];
    let value = bytes / 1024;
    let unit = 0;
    while (value >= 1024 && unit < units.length - 1) {
      value /= 1024;
      unit += 1;
    }
    return `${value.toFixed(value >= 10 ? 0 : 1)} ${units[unit]}`;
  };

  document.addEventListener("change", (event) => {
    const input = event.target;
    if (!(input instanceof HTMLInputElement) || !input.matches(".file-input")) return;

    const form = input.closest(".upload");
    const title = form.querySelector(".picker-title");
    const detail = form.querySelector(".picker-detail");
    const list = form.querySelector(".selected-files");
    const files = Array.from(input.files || []);

    list.replaceChildren();
    list.classList.toggle("has-files", files.length > 0);
    title.textContent = files.length === 1 ? files[0].name : files.length > 1 ? `${files.length} files selected` : "Choose files";
    detail.textContent = files.length ? `${formatSize(files.reduce((total, file) => total + file.size, 0))} total` : "No files selected";

    files.forEach((file) => {
      const item = document.createElement("li");
      item.className = "selected-file";

      const name = document.createElement("span");
      name.className = "selected-name";
      name.textContent = file.name;

      const size = document.createElement("span");
      size.className = "selected-size";
      size.textContent = formatSize(file.size);

      item.append(name, size);
      list.append(item);
    });
  });
})();
