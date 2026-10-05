SELECT department,
       SUM(total_amount) AS department_amount,
       SUM(SUM(total_amount)) OVER () AS total_2026_amount,
       100.0 * SUM(total_amount) / SUM(SUM(total_amount)) OVER () AS pct_of_total
FROM expense_summary
WHERE month >= '2026-01' AND month <= '2026-12'
GROUP BY department
ORDER BY pct_of_total DESC
