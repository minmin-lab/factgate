
SELECT department,
       SUM(CASE WHEN month = '2026-06' THEN total_amount ELSE 0 END) - SUM(CASE WHEN month = '2026-05' THEN total_amount ELSE 0 END) AS amount_diff
FROM expense_summary
WHERE month = '2026-06' OR month = '2026-05'
GROUP BY department

