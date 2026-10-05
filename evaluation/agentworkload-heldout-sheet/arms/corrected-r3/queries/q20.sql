SELECT department,
       COALESCE(SUM(CASE WHEN month = '2026-06' THEN total_amount END), 0) - COALESCE(SUM(CASE WHEN month = '2026-05' THEN total_amount END), 0) AS amount_diff
FROM expense_summary
WHERE month = '2026-06' OR month = '2026-05'
GROUP BY department
ORDER BY department
