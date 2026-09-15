fetch("/api/v1/health")
  .then((r) => r.json())
  .then((data) => {
    for (const link of document.querySelectorAll("[data-feature]"))
      link.hidden = !data.features?.[link.dataset.feature];
  })
  .catch(() => {});
