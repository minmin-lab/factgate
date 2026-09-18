
SELECT department,
       SUM(total_amount) AS department_amount,
       SUM(total_amount) * 100 / (SELECT SUM(total_amount)
                                  FROM expense_summary
                                  WHERE month >= '2026-01' AND month <= '2026-12') AS pct_of_total
FROM expense_summary
WHERE month >= '2026-01' AND month <= '2026-12'
GROUP BY department
ORDER BY pct_of_total DESC

