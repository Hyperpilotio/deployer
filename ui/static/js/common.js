var $deployments = $('.deployment-list')
$(function() {
    if ($deployments) {
        fixDeploymentHeight();
        $('.deployment-list li').click(function() {
            $('.deployment-list li').removeClass('active');
            $(this).addClass('active');
            $('main h1').remove();
        });
    }
});

function fixDeploymentHeight() {
    var realHeight = $deployments.height();
    $deployments.css('height', (realHeight + 100) + 'px');
}